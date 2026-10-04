package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Opening a shared watch is a network round trip of unknown length: a client-go watch has no
// response-header timeout of its own. These tests hold the upstream inside Watch, on a barrier, and
// prove what must stay true meanwhile: other scopes proceed, every waiter can leave, the last to leave
// cancels the opening, and an opening that comes back after everyone left changes nothing.

// gatedUpstream blocks every Watch until the test releases it, announcing each call first.
type gatedUpstream struct {
	calls chan *gatedOpen
	// ignoreCancel makes Watch deaf to its context, as an upstream that does not honour cancellation
	// is. Its result then arrives whenever the test releases it, however late.
	ignoreCancel bool
}

func newGatedUpstream() *gatedUpstream { return &gatedUpstream{calls: make(chan *gatedOpen, 64)} }

type gatedOpen struct {
	scope    Scope
	ctx      context.Context
	release  chan gatedResult
	returned chan struct{}
}

type gatedResult struct {
	w   Watcher
	err error
}

func (u *gatedUpstream) Watch(ctx context.Context, scope Scope) (Watcher, error) {
	o := &gatedOpen{scope: scope, ctx: ctx, release: make(chan gatedResult, 1), returned: make(chan struct{})}
	defer close(o.returned)
	u.calls <- o
	cancelled := ctx.Done()
	if u.ignoreCancel {
		cancelled = nil
	}
	select {
	case r := <-o.release:
		return r.w, r.err
	case <-cancelled:
		return nil, ctx.Err()
	}
}

// opened waits for the next upstream Watch call.
func (u *gatedUpstream) opened(t *testing.T) *gatedOpen {
	t.Helper()
	select {
	case o := <-u.calls:
		return o
	case <-time.After(2 * time.Second):
		t.Fatal("the upstream was never asked to open")
		return nil
	}
}

// quiet asserts that no further upstream Watch call has been made.
func (u *gatedUpstream) quiet(t *testing.T) {
	t.Helper()
	select {
	case o := <-u.calls:
		t.Fatalf("an unexpected upstream opening of %+v", o.scope)
	default:
	}
}

// openWatcher is the watcher an opening returns: fed by the test, and recording that it was stopped.
type openWatcher struct {
	events  chan WatchEvent
	stopped chan struct{}
	once    sync.Once
}

func newOpenWatcher() *openWatcher {
	return &openWatcher{events: make(chan WatchEvent, 16), stopped: make(chan struct{})}
}

func (w *openWatcher) Next(ctx context.Context) (WatchEvent, error) {
	select {
	case ev := <-w.events:
		return ev, nil
	case <-ctx.Done():
		return WatchEvent{}, ctx.Err()
	}
}

func (w *openWatcher) Stop() { w.once.Do(func() { close(w.stopped) }) }

func within(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal(what)
	}
}

// watchResult is one asynchronous SharedBackend.Watch call.
type watchResult struct {
	w   Watcher
	err error
}

func watchAsync(ctx context.Context, b *SharedBackend, scope Scope) <-chan watchResult {
	out := make(chan watchResult, 1)
	go func() {
		w, err := b.Watch(ctx, scope)
		out <- watchResult{w, err}
	}()
	return out
}

func await(t *testing.T, ch <-chan watchResult) watchResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return")
		return watchResult{}
	}
}

func pending(t *testing.T, ch <-chan watchResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("Watch returned (%v, %v) while its opening was still in progress", r.w, r.err)
	default:
	}
}

// waitForWaiters blocks until an opening of scope has n callers waiting on it. Polling a condition,
// not sleeping for an ordering: it returns the moment the condition holds.
func waitForWaiters(t *testing.T, b *SharedBackend, scope Scope, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		got := 0
		if op := b.openings[scopeKey(scope)]; op != nil {
			got = op.waiters
		}
		b.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters = %d, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func (b *SharedBackend) state() (scopes, openings int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.scopes), len(b.openings)
}

var otherScopeUnderTest = Scope{Version: "v1", Resource: "configmaps", Namespace: "other"}

// The field report's first failure: one API server slow to answer one opening held the backend-wide
// lock, and with it every other scope's stream. Now an opening holds up only its own scope.
func TestASlowOpeningDoesNotHoldUpAnotherScope(t *testing.T) {
	up := newGatedUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	ctxA, cancelA := context.WithCancel(t.Context())
	defer cancelA()
	a := watchAsync(ctxA, b, sharedScopeUnderTest)
	openA := up.opened(t) // scope A is now inside the upstream, and stays there

	bres := watchAsync(t.Context(), b, otherScopeUnderTest)
	openB := up.opened(t)
	if openB.scope != otherScopeUnderTest {
		t.Fatalf("opened %+v, want scope B", openB.scope)
	}
	wb := newOpenWatcher()
	openB.release <- gatedResult{w: wb}
	got := await(t, bres)
	if got.err != nil {
		t.Fatalf("scope B: %v", got.err)
	}
	defer got.w.Stop()
	wb.events <- WatchEvent{Type: WatchAdded, Object: obj("uid-b", "cm-b", "1")}
	wb.events <- boundary
	if uids := drainSnapshot(t, got.w); !uids["uid-b"] {
		t.Fatalf("scope B snapshot = %v, want uid-b", uids)
	}

	pending(t, a)
	cancelA()
	if r := await(t, a); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("scope A = %v, want its caller's cancellation", r.err)
	}
	within(t, openA.returned, "the abandoned opening was not cancelled")
}

// Ten tabs arriving while a scope opens are still one upstream watch, and each is attached to it.
func TestConcurrentCallersForOneScopeShareOneOpening(t *testing.T) {
	up := newGatedUpstream()
	var mu sync.Mutex
	var opened, closed int
	b := NewSharedBackend(up, SharedOptions{Observer: ObserverFunc(func(o Observation) {
		mu.Lock()
		defer mu.Unlock()
		switch o.Kind {
		case ObservationSharedSubscriptionOpened:
			opened++
		case ObservationSharedSubscriptionClosed:
			closed++
		}
	})})

	var results []<-chan watchResult
	for range 10 {
		results = append(results, watchAsync(t.Context(), b, sharedScopeUnderTest))
	}
	open := up.opened(t)
	waitForWaiters(t, b, sharedScopeUnderTest, 10)
	up.quiet(t)

	w := newOpenWatcher()
	open.release <- gatedResult{w: w}
	var watchers []Watcher
	for _, r := range results {
		got := await(t, r)
		if got.err != nil {
			t.Fatalf("Watch: %v", got.err)
		}
		watchers = append(watchers, got.w)
	}
	up.quiet(t)

	w.events <- WatchEvent{Type: WatchAdded, Object: obj("uid-a", "cm-a", "1")}
	w.events <- boundary
	for i, sw := range watchers {
		if uids := drainSnapshot(t, sw); !uids["uid-a"] {
			t.Errorf("subscriber %d snapshot = %v, want uid-a", i, uids)
		}
	}
	for _, sw := range watchers {
		sw.Stop()
	}
	within(t, w.stopped, "the last subscriber left and the upstream watch stayed open")
	mu.Lock()
	defer mu.Unlock()
	if opened != 10 || closed != 10 {
		t.Errorf("subscriptions opened %d, closed %d; want 10 and 10", opened, closed)
	}
}

// A browser that leaves while its scope is opening gets its request back at once; the others keep
// waiting on the same opening, which is not cancelled on their behalf.
func TestCancellingOneWaiterLeavesTheOthersWaiting(t *testing.T) {
	up := newGatedUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	leaving, leave := context.WithCancel(t.Context())
	defer leave()
	first := watchAsync(leaving, b, sharedScopeUnderTest)
	open := up.opened(t)
	staying := watchAsync(t.Context(), b, sharedScopeUnderTest)
	waitForWaiters(t, b, sharedScopeUnderTest, 2)

	leave()
	if r := await(t, first); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("the departing waiter got %v, want its own cancellation", r.err)
	}
	waitForWaiters(t, b, sharedScopeUnderTest, 1)
	if open.ctx.Err() != nil {
		t.Fatal("one waiter leaving cancelled an opening another still needs")
	}
	pending(t, staying)

	w := newOpenWatcher()
	open.release <- gatedResult{w: w}
	got := await(t, staying)
	if got.err != nil {
		t.Fatalf("the remaining waiter: %v", got.err)
	}
	defer got.w.Stop()
	w.events <- boundary
	drainSnapshot(t, got.w)
	up.quiet(t)
}

// The last waiter out cancels the opening and unregisters it, without recording a failure: nobody
// waiting is not the upstream being away. The next caller opens afresh, at once.
func TestCancellingTheLastWaiterCancelsTheOpening(t *testing.T) {
	up := newGatedUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	ctx1, cancel1 := context.WithCancel(t.Context())
	ctx2, cancel2 := context.WithCancel(t.Context())
	defer cancel1()
	defer cancel2()
	r1 := watchAsync(ctx1, b, sharedScopeUnderTest)
	open := up.opened(t)
	r2 := watchAsync(ctx2, b, sharedScopeUnderTest)
	waitForWaiters(t, b, sharedScopeUnderTest, 2)

	cancel1()
	await(t, r1)
	cancel2()
	await(t, r2)
	within(t, open.ctx.Done(), "the last waiter left and the upstream opening's context was not cancelled")
	within(t, open.returned, "the cancelled opening never returned")
	if scopes, openings := b.state(); scopes != 0 || openings != 0 {
		t.Fatalf("scopes=%d openings=%d after every waiter left, want none", scopes, openings)
	}
	b.mu.Lock()
	_, backingOff := b.backoff[scopeKey(sharedScopeUnderTest)]
	b.mu.Unlock()
	if backingOff {
		t.Fatal("an opening abandoned by its waiters was recorded as an upstream failure")
	}

	retry := watchAsync(t.Context(), b, sharedScopeUnderTest)
	again := up.opened(t)
	w := newOpenWatcher()
	again.release <- gatedResult{w: w}
	got := await(t, retry)
	if got.err != nil {
		t.Fatalf("retry: %v", got.err)
	}
	got.w.Stop()
}

// A caller that is already gone starts nothing, joins nothing, and holds nothing open.
func TestACancelledCallerDoesNotOpenOrJoin(t *testing.T) {
	up := newGatedUpstream()
	b := NewSharedBackend(up, SharedOptions{})
	gone, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := b.Watch(gone, sharedScopeUnderTest); !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch = %v, want the caller's cancellation", err)
	}
	up.quiet(t)
	if scopes, openings := b.state(); scopes != 0 || openings != 0 {
		t.Fatalf("scopes=%d openings=%d, want none", scopes, openings)
	}

	// Nor does it join an opening already under way, and so cannot keep it alive.
	ctx, leave := context.WithCancel(t.Context())
	r := watchAsync(ctx, b, sharedScopeUnderTest)
	open := up.opened(t)
	waitForWaiters(t, b, sharedScopeUnderTest, 1)
	if _, err := b.Watch(gone, sharedScopeUnderTest); !errors.Is(err, context.Canceled) {
		t.Fatalf("Watch = %v, want the caller's cancellation", err)
	}
	waitForWaiters(t, b, sharedScopeUnderTest, 1)
	leave()
	await(t, r)
	within(t, open.ctx.Done(), "the opening outlived its only real waiter")
}

// A waiter's cancellation and the opening's success can land together, and either may be noticed
// first. Whichever is, nothing is left behind: no subscription, no scope, no upstream watch, and every
// attachment observed as opened is observed as closed.
func TestCancellationRacingASuccessfulOpeningOrphansNothing(t *testing.T) {
	for i := range 200 {
		up := newGatedUpstream()
		up.ignoreCancel = true // so the released watcher always reaches the backend, which must dispose of it
		var mu sync.Mutex
		balance := 0
		b := NewSharedBackend(up, SharedOptions{Observer: ObserverFunc(func(o Observation) {
			mu.Lock()
			defer mu.Unlock()
			switch o.Kind {
			case ObservationSharedSubscriptionOpened:
				balance++
			case ObservationSharedSubscriptionClosed:
				balance--
			}
		})})

		ctx, cancel := context.WithCancel(t.Context())
		r := watchAsync(ctx, b, sharedScopeUnderTest)
		open := up.opened(t)
		waitForWaiters(t, b, sharedScopeUnderTest, 1)

		w := newOpenWatcher()
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() { <-start; open.release <- gatedResult{w: w} })
		wg.Go(func() { <-start; cancel() })
		close(start)
		wg.Wait()

		if got := await(t, r); got.err == nil {
			got.w.Stop() // the opening won: the caller owns a subscriber, and releases it as usual
		} else if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("iteration %d: Watch = %v", i, got.err)
		}
		within(t, w.stopped, "the upstream watch was orphaned")
		if scopes, openings := b.state(); scopes != 0 || openings != 0 {
			t.Fatalf("iteration %d: scopes=%d openings=%d left behind", i, scopes, openings)
		}
		mu.Lock()
		if balance != 0 {
			t.Fatalf("iteration %d: %d subscriptions opened and never closed", i, balance)
		}
		mu.Unlock()
	}
}

// An upstream that ignores cancellation can return long after its waiters left, by which time a new
// opening may own the scope. Its result is discarded: a watcher is stopped, a failure records no
// backoff, and the newer opening is neither removed, nor populated, nor slowed.
func TestAnAbandonedOpeningReturningLateCannotTouchItsReplacement(t *testing.T) {
	for name, late := range map[string]func() gatedResult{
		"with a watcher": func() gatedResult { return gatedResult{w: newOpenWatcher()} },
		"with a failure": func() gatedResult {
			return gatedResult{err: UpstreamUnavailable("the API server is unavailable", time.Minute)}
		},
		"with a cancellation": func() gatedResult { return gatedResult{err: context.Canceled} },
	} {
		t.Run(name, func(t *testing.T) {
			up := newGatedUpstream()
			up.ignoreCancel = true
			b := NewSharedBackend(up, SharedOptions{})

			ctx, leave := context.WithCancel(t.Context())
			abandoned := watchAsync(ctx, b, sharedScopeUnderTest)
			old := up.opened(t)
			waitForWaiters(t, b, sharedScopeUnderTest, 1)
			leave()
			await(t, abandoned)

			replacement := watchAsync(t.Context(), b, sharedScopeUnderTest)
			current := up.opened(t)
			waitForWaiters(t, b, sharedScopeUnderTest, 1)

			result := late()
			old.release <- result
			within(t, old.returned, "the old opening did not return")
			if w, ok := result.w.(*openWatcher); ok {
				within(t, w.stopped, "the late watcher was kept rather than stopped")
			}
			// The replacement is still registered, still waited for, and nothing is backing off.
			waitForWaiters(t, b, sharedScopeUnderTest, 1)
			b.mu.Lock()
			scopes, backoff := len(b.scopes), len(b.backoff)
			b.mu.Unlock()
			if scopes != 0 || backoff != 0 {
				t.Fatalf("the late result changed state: scopes=%d backoff=%d", scopes, backoff)
			}
			pending(t, replacement)

			w := newOpenWatcher()
			current.release <- gatedResult{w: w}
			got := await(t, replacement)
			if got.err != nil {
				t.Fatalf("replacement: %v", got.err)
			}
			defer got.w.Stop()
			w.events <- WatchEvent{Type: WatchAdded, Object: obj("uid-new", "cm", "2")}
			w.events <- boundary
			if uids := drainSnapshot(t, got.w); len(uids) != 1 || !uids["uid-new"] {
				t.Fatalf("snapshot = %v, want only the replacement's uid-new", uids)
			}
		})
	}
}

// A genuine failure to open is classified once — one backoff, however many were waiting — and every
// waiter receives it as it is. Terminal refusals reach them the same way, and do not back off.
func TestAnOpeningFailureReachesEveryWaiterAndBacksOffOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		err       *StreamError
		backsOff  bool
		wantDelay time.Duration
	}{
		"retryable with a hint": {UpstreamUnavailable("the API server is unavailable", 5*time.Second), true, 5 * time.Second},
		"retryable":             {UpstreamUnavailable("the API server is unavailable", 0), true, sharedRetryMin},
		"terminal":              {Forbidden("the gateway's identity may not watch this scope"), false, 0},
	} {
		t.Run(name, func(t *testing.T) {
			up := newGatedUpstream()
			b := NewSharedBackend(up, SharedOptions{})
			now := time.Unix(0, 0)
			b.now = func() time.Time { return now } // read only under b.mu, after the waiters arrived

			var results []<-chan watchResult
			for range 3 {
				results = append(results, watchAsync(t.Context(), b, sharedScopeUnderTest))
			}
			open := up.opened(t)
			waitForWaiters(t, b, sharedScopeUnderTest, 3)
			open.release <- gatedResult{err: tc.err}
			for i, r := range results {
				var se *StreamError
				if got := await(t, r); !errors.As(got.err, &se) || se != tc.err {
					t.Fatalf("waiter %d got %v, want the opening's own %v", i, got.err, tc.err)
				}
			}
			if !errors.Is(open.ctx.Err(), context.Canceled) {
				t.Error("a failed opening's context was left uncancelled")
			}

			b.mu.Lock()
			bo := b.backoff[scopeKey(sharedScopeUnderTest)]
			b.mu.Unlock()
			if !tc.backsOff {
				if bo != nil {
					t.Fatalf("a terminal refusal backed off: %+v", bo)
				}
				return
			}
			if bo == nil || bo.delay != sharedRetryMin || bo.until.Sub(now) != tc.wantDelay {
				t.Fatalf("backoff = %+v, want one failure recorded, waiting %v", bo, tc.wantDelay)
			}
			_, err := b.Watch(t.Context(), sharedScopeUnderTest)
			var se *StreamError
			if !errors.As(err, &se) || se.Code != CodeUpstreamUnavailable || se.RetryAfterMs == nil {
				t.Fatalf("Watch during the backoff = %v, want UPSTREAM_UNAVAILABLE with a hint", err)
			}
			up.quiet(t)
		})
	}
}

// Seen from the stream loop: a stream whose scope is slow to open ends when its caller leaves, does
// not hold up a stream of another scope, and does not leave the opening behind.
func TestAStreamWaitingOnASlowOpeningEndsWithItsCaller(t *testing.T) {
	up := newGatedUpstream()
	b := NewSharedBackend(up, SharedOptions{})
	var mu sync.Mutex
	var kinds []ObservationKind
	g := &Gateway{
		StreamConfig: StreamConfig{
			Authorizer: AllowAll{},
			Clients:    func(context.Context, string, Principal) (Backend, error) { return b, nil },
			Observer: ObserverFunc(func(o Observation) {
				mu.Lock()
				defer mu.Unlock()
				if o.Scope == sharedScopeUnderTest {
					kinds = append(kinds, o.Kind)
				}
			}),
		},
	}

	slowCtx, leave := context.WithCancel(t.Context())
	defer leave()
	slowDone := make(chan error, 1)
	go func() { slowDone <- g.Stream(slowCtx, nil, sharedScopeUnderTest, "", &lockedSink{}) }()
	slowOpen := up.opened(t)

	fast := make(reauthEvents, 8)
	fastCtx, stopFast := context.WithCancel(t.Context())
	defer stopFast()
	fastDone := make(chan error, 1)
	go func() { fastDone <- g.Stream(fastCtx, nil, otherScopeUnderTest, "", fast) }()
	fastOpen := up.opened(t)
	w := newOpenWatcher()
	fastOpen.release <- gatedResult{w: w}
	w.events <- boundary
	awaitEvent(t, fast, EventSynced)

	leave()
	select {
	case err := <-slowDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("slow stream = %v, want its caller's cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stream waiting on a slow opening outlived its caller")
	}
	within(t, slowOpen.returned, "the slow opening was not cancelled")
	stopFast()
	<-fastDone
	within(t, w.stopped, "the fast stream's upstream outlived it")
	mu.Lock()
	defer mu.Unlock()
	for _, k := range kinds {
		if k == ObservationRetryableError || k == ObservationTerminalError {
			t.Errorf("a caller leaving was reported as %s", k)
		}
	}
}
