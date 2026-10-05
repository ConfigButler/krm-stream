package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// SharedBackend is concurrent, and concurrent code that is only tested for its happy path is
// untested. So: the sharing itself, the warm cache a joiner gets for free, the backpressure that
// stops one stalled tab from stalling everyone, and the teardown — because a shared watch that
// outlives its audience is a leak with a cache attached.

// fakeUpstream is one controllable watch, and it COUNTS how many times it was opened. That count is
// the entire point of fan-out, so it is the thing most worth asserting.
type fakeUpstream struct {
	mu     sync.Mutex
	opened int
	ch     chan WatchEvent
	closed bool
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{ch: make(chan WatchEvent, 64)}
}

func (f *fakeUpstream) Watch(context.Context, Scope) (Watcher, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened++
	return &fakeUpstreamWatcher{f: f}, nil
}

func (f *fakeUpstream) opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opened
}

// send pushes one event into the upstream.
func (f *fakeUpstream) send(ev WatchEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.ch <- ev
	}
}

type fakeUpstreamWatcher struct{ f *fakeUpstream }

func (w *fakeUpstreamWatcher) Next(ctx context.Context) (WatchEvent, error) {
	select {
	case <-ctx.Done():
		return WatchEvent{}, ctx.Err()
	case ev := <-w.f.ch:
		return ev, nil
	}
}
func (w *fakeUpstreamWatcher) Stop() {}

func obj(uid, name, rv string) KRMObject {
	return KRMObject{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"uid": uid, "name": name, "namespace": "app", "resourceVersion": rv,
		},
	}
}

var sharedScopeUnderTest = Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}

// next pulls one event, failing rather than hanging forever.
func next(t *testing.T, w Watcher) WatchEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ev, err := w.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

// drainSnapshot reads added* up to the boundary bookmark and returns the uids it saw.
func drainSnapshot(t *testing.T, w Watcher) map[string]bool {
	t.Helper()
	uids := map[string]bool{}
	for {
		ev := next(t, w)
		switch ev.Type {
		case WatchAdded:
			uids[ev.Object.UID()] = true
		case WatchBookmark:
			if ev.InitialEventsEnd {
				return uids
			}
		default:
			t.Fatalf("unexpected event during snapshot: %+v", ev)
		}
	}
}

// The headline: ten tabs, one watch.
func TestSharedBackendOpensOneUpstreamForManySubscribers(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	first, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer first.Stop()

	// The upstream snapshot completes.
	up.send(WatchEvent{Type: WatchAdded, Object: obj("uid-a", "cm-a", "10")})
	up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})

	if got := drainSnapshot(t, first); !got["uid-a"] {
		t.Fatalf("first subscriber's snapshot = %v, want uid-a", got)
	}

	// Nine more tabs on the same scope.
	watchers := []Watcher{}
	for i := range 9 {
		w, err := b.Watch(t.Context(), sharedScopeUnderTest)
		if err != nil {
			t.Fatalf("subscriber %d: %v", i, err)
		}
		defer w.Stop()
		watchers = append(watchers, w)

		// Each gets its whole reset…synced from the WARM CACHE — no upstream call, no API server.
		if got := drainSnapshot(t, w); !got["uid-a"] {
			t.Errorf("subscriber %d snapshot = %v, want uid-a from the warm cache", i, got)
		}
	}

	if up.opens() != 1 {
		t.Errorf("the upstream was opened %d times for 10 subscribers, want 1 — that is the whole point", up.opens())
	}

	// …and one live event reaches every one of them.
	up.send(WatchEvent{Type: WatchModified, Object: obj("uid-a", "cm-a", "11")})
	for i, w := range watchers {
		ev := next(t, w)
		if ev.Type != WatchModified || ev.Object.UID() != "uid-a" {
			t.Errorf("subscriber %d got %+v, want modified uid-a", i, ev)
		}
	}
}

// A DIFFERENT scope is a different watch. Merging two scopes would hand a consumer objects it never
// asked for — including, potentially, ones it may not see.
func TestDifferentScopesAreNotShared(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	a, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer a.Stop()

	other := sharedScopeUnderTest
	other.LabelSelector = "tier=web" // a selector changes WHICH objects are in scope
	c, err := b.Watch(t.Context(), other)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer c.Stop()

	if up.opens() != 2 {
		t.Errorf("upstream opens = %d, want 2: a label selector is part of the scope", up.opens())
	}
}

// A subscriber that cannot keep up must not be able to stall the pump — and therefore everyone else.
// It is resnapshotted instead, off the warm cache, which costs the API server nothing.
func TestASlowSubscriberIsResnapshottedNotBlocking(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	slow, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer slow.Stop()
	fast, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer fast.Stop()

	up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	drainSnapshot(t, slow)
	drainSnapshot(t, fast)

	// `slow` never reads again. Flood it well past its queue depth.
	for i := range sharedQueueDepth * 2 {
		up.send(WatchEvent{Type: WatchModified, Object: obj("uid-a", "cm-a", fmt.Sprint(i))})
	}

	// The FAST subscriber still gets events — the pump was never blocked by the slow one.
	deadline := time.After(3 * time.Second)
	got := 0
	for got < 10 {
		select {
		case <-deadline:
			t.Fatalf("the fast subscriber received only %d events: a slow consumer stalled the pump", got)
		default:
		}
		ev, err := fast.Next(t.Context())
		if err != nil {
			t.Fatalf("fast.Next: %v", err)
		}
		if ev.Type == WatchModified {
			got++
		}
	}

	// …and the slow one is told to resync rather than being silently starved or lied to.
	//
	// It may hear it either way, and both are the same thing to the stream loop: as a WatchError
	// event, or as a non-terminal *StreamError from Next (which is how the reason survives a queue
	// too full to hold it — see subscriber.reason). What must NOT happen is a bare close with no
	// reason, or events simply going missing.
	sawResync := false
	for range sharedQueueDepth + 2 {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		ev, err := slow.Next(ctx)
		cancel()

		var se *StreamError
		switch {
		case errors.As(err, &se) && se.Code == CodeResyncRequired && !se.Terminal:
			sawResync = true
		case err != nil:
			t.Fatalf("the slow subscriber was ended with %v — a bare close tells nobody WHY", err)
		case ev.Type == WatchError:
			sawResync = errors.As(ev.Err, &se) && se.Code == CodeResyncRequired
		}
		if sawResync {
			break
		}
	}
	if !sawResync {
		t.Error("the slow subscriber was never told it had fallen behind — it would silently miss events")
	}
}

func TestSharedBackendOptionsBoundQueueAndReportOverflow(t *testing.T) {
	up := newFakeUpstream()
	overflow := make(chan Observation, 1)
	b := NewSharedBackend(up, SharedOptions{
		QueueDepth: 1,
		Observer: ObserverFunc(func(observation Observation) {
			if observation.Kind == ObservationSharedOverflow {
				overflow <- observation
			}
		}),
	})

	w, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()
	up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	drainSnapshot(t, w)

	up.send(WatchEvent{Type: WatchModified, Object: obj("uid-a", "cm-a", "1")})
	up.send(WatchEvent{Type: WatchModified, Object: obj("uid-a", "cm-a", "2")})

	select {
	case observation := <-overflow:
		if observation.Scope != sharedScopeUnderTest {
			t.Errorf("overflow scope = %+v, want %+v", observation.Scope, sharedScopeUnderTest)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queue overflow was not observed")
	}
}

// A partial object must never enter the cache. The stream loop guards its own output, but this cache
// is REPLAYED to every future joiner: a husk forwarded once blanks one consumer's object; a husk
// CACHED is served to everybody who arrives later, for as long as the scope lives.
func TestAPartialObjectPoisonsNothing(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	w, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	drainSnapshot(t, w)

	// A PartialObjectMetadata: a uid, but no spec/status. Forwarding or caching it blanks the object.
	up.send(WatchEvent{Type: WatchAdded, Object: KRMObject{
		"apiVersion": "meta.k8s.io/v1",
		"kind":       "PartialObjectMetadata",
		"metadata":   map[string]any{"uid": "uid-a", "name": "cm-a"},
	}})

	ev := next(t, w)
	if ev.Type != WatchError {
		t.Fatalf("a partial object was forwarded as %v — it would BLANK the consumer's object", ev.Type)
	}
	var serr *StreamError
	if !errors.As(ev.Err, &serr) || serr.Code != CodeResyncRequired {
		t.Errorf("err = %v, want RESYNC_REQUIRED", ev.Err)
	}

	// And the scope is gone, so the next Watch builds a clean one rather than inheriting the poison.
	b.mu.Lock()
	n := len(b.scopes)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("the poisoned scope survived (%d live) — its cache would be served to the next joiner", n)
	}
}

// The last subscriber out turns off the lights. A shared watch nobody is watching is a goroutine, a
// connection and a cache, all held open forever.
func TestTheLastSubscriberOutStopsTheUpstream(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	a, _ := b.Watch(t.Context(), sharedScopeUnderTest)
	c, _ := b.Watch(t.Context(), sharedScopeUnderTest)

	a.Stop()
	b.mu.Lock()
	n := len(b.scopes)
	b.mu.Unlock()
	if n != 1 {
		t.Fatalf("the scope was dropped while a subscriber remained (%d live)", n)
	}

	c.Stop()
	b.mu.Lock()
	n = len(b.scopes)
	b.mu.Unlock()
	if n != 0 {
		t.Errorf("the last subscriber left and the upstream watch is still open (%d live)", n)
	}

	// A new subscriber gets a genuinely fresh watch, not a corpse.
	d, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch after teardown: %v", err)
	}
	defer d.Stop()
	if up.opens() != 2 {
		t.Errorf("upstream opens = %d, want 2 (one torn down, one fresh)", up.opens())
	}
}

// Upstream continuity loss reaches EVERY subscriber, and the N of them resyncing at once still
// produce exactly one new upstream watch — not N.
func TestUpstreamErrorFansOutAndDoesNotStampede(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up, SharedOptions{})

	var ws []Watcher
	for range 5 {
		w, err := b.Watch(t.Context(), sharedScopeUnderTest)
		if err != nil {
			t.Fatalf("Watch: %v", err)
		}
		ws = append(ws, w)
	}
	up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	for _, w := range ws {
		drainSnapshot(t, w)
	}

	// A 410 upstream.
	up.send(WatchEvent{Type: WatchError, Err: ResyncRequired("the upstream resourceVersion expired (410 Gone)")})

	for i, w := range ws {
		ev := next(t, w)
		if ev.Type != WatchError {
			t.Errorf("subscriber %d got %v, want the error — it would keep a ghost forever", i, ev.Type)
		}
		w.Stop()
	}

	// All five stream loops now begin a new cycle at once. That must be ONE upstream watch, not five.
	var fresh []Watcher
	for range 5 {
		w, err := b.Watch(t.Context(), sharedScopeUnderTest)
		if err != nil {
			t.Fatalf("re-Watch: %v", err)
		}
		fresh = append(fresh, w)
	}
	defer func() {
		for _, w := range fresh {
			w.Stop()
		}
	}()

	if up.opens() != 2 {
		t.Errorf("upstream opens = %d, want 2: five subscribers resyncing at once caused a stampede", up.opens())
	}
}

// A shared upstream has no browser to hand its retry to, and each of its subscribers reconnects on
// its own budget. However many there are, the scope sees one open attempt per backoff period; the
// rest are told how long to wait.
func TestSharedBackendBacksOffAnUnavailableUpstream(t *testing.T) {
	opens := 0
	up := backendFunc(func() (Watcher, error) {
		opens++
		return nil, UpstreamUnavailable("the API server is unavailable", 0)
	})
	b := NewSharedBackend(up, SharedOptions{})
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	scope := Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}

	retryAfter := func() int {
		t.Helper()
		_, err := b.Watch(t.Context(), scope)
		var se *StreamError
		if !errors.As(err, &se) || se.Code != CodeUpstreamUnavailable || se.Terminal {
			t.Fatalf("Watch() = %v, want UPSTREAM_UNAVAILABLE", err)
		}
		if se.RetryAfterMs == nil {
			return 0
		}
		return *se.RetryAfterMs
	}

	retryAfter()
	for range 50 { // a reconnect storm within the first second
		if ms := retryAfter(); ms <= 0 || ms > 1000 {
			t.Fatalf("retryAfterMs = %d, want the remaining backoff", ms)
		}
	}
	if opens != 1 {
		t.Fatalf("upstream opens = %d, want 1 during the backoff", opens)
	}

	now = now.Add(time.Second)
	retryAfter()
	if opens != 2 {
		t.Fatalf("upstream opens = %d, want a second attempt once the backoff expired", opens)
	}
	if ms := retryAfter(); ms != 2000 {
		t.Errorf("retryAfterMs = %d, want the backoff doubled to 2000", ms)
	}
}

// dyingWatcher opens fine and then fails at once: through Next, or as a WatchError event.
type dyingWatcher struct {
	err     error
	asEvent bool
}

func (w dyingWatcher) Next(context.Context) (WatchEvent, error) {
	if w.asEvent {
		return WatchEvent{Type: WatchError, Err: w.err}, nil
	}
	return WatchEvent{}, w.err
}
func (dyingWatcher) Stop() {}

// A watch that opens and then dies of an unavailable upstream is the same outage as one that fails
// to open, and must be bounded the same way: one upstream attempt per backoff period, however many
// subscribers reconnect at once, honouring the upstream's hint.
func TestSharedBackendBacksOffAWatchThatDiesAfterOpening(t *testing.T) {
	for _, asEvent := range []bool{false, true} {
		t.Run(map[bool]string{false: "Next error", true: "WatchError event"}[asEvent], func(t *testing.T) {
			var mu sync.Mutex
			opens := 0
			up := backendFunc(func() (Watcher, error) {
				mu.Lock()
				opens++
				mu.Unlock()
				return dyingWatcher{err: UpstreamUnavailable("the API server is unavailable", 10*time.Second), asEvent: asEvent}, nil
			})
			b := NewSharedBackend(up, SharedOptions{})
			var clock sync.Mutex
			now := time.Unix(0, 0)
			b.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
			scope := Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}

			// What a subscriber's stream loop does: resync in band on a closed watch, and give up on
			// anything else (the gateway sends it and closes).
			reconnect := func() *StreamError {
				for {
					w, err := b.Watch(t.Context(), scope)
					if err == nil {
						for err == nil {
							var ev WatchEvent
							ev, err = w.Next(t.Context())
							if err == nil && ev.Type == WatchError {
								err = ev.Err
							}
						}
						w.Stop()
					}
					var se *StreamError
					if errors.As(err, &se) && se.Code == CodeUpstreamUnavailable {
						return se
					}
					if !errors.Is(err, ErrWatchClosed) && (se == nil || se.Code != CodeResyncRequired) {
						t.Errorf("unexpected error: %v", err)
						return nil
					}
				}
			}

			var wg sync.WaitGroup
			hints := make(chan int, 100)
			for range 100 {
				wg.Go(func() {
					if se := reconnect(); se != nil && se.RetryAfterMs != nil {
						hints <- *se.RetryAfterMs
					}
				})
			}
			wg.Wait()
			close(hints)
			if opens != 1 {
				t.Fatalf("upstream opens = %d, want 1 for 100 concurrent reconnects", opens)
			}
			for ms := range hints {
				if ms <= 0 || ms > 10_000 {
					t.Fatalf("retryAfterMs = %d, want the upstream's 10s hint or what remains of it", ms)
				}
			}

			clock.Lock()
			now = now.Add(9 * time.Second)
			clock.Unlock()
			_ = reconnect()
			if opens != 1 {
				t.Fatalf("upstream opens = %d, want the 10s hint honoured", opens)
			}
			clock.Lock()
			now = now.Add(time.Second)
			clock.Unlock()
			_ = reconnect()
			if opens != 2 {
				t.Fatalf("upstream opens = %d, want one attempt once the hint expired", opens)
			}
		})
	}
}

// Obtaining a watcher is not recovery; a completed upstream snapshot is. Only then does the backoff
// reset, so a flapping upstream keeps doubling it.
func TestSharedBackoffResetsOnlyAfterAUsefulWatch(t *testing.T) {
	healthy := false
	up := backendFunc(func() (Watcher, error) {
		if healthy {
			return &stubWatcher{events: []WatchEvent{{Type: WatchBookmark, InitialEventsEnd: true}}}, nil
		}
		return dyingWatcher{err: UpstreamUnavailable("the API server is unavailable", 0)}, nil
	})
	b := NewSharedBackend(up, SharedOptions{})
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	scope := Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}
	key := scopeKey(scope)

	for i := range 3 {
		w, err := b.Watch(t.Context(), scope)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		for err == nil {
			_, err = w.Next(t.Context())
		}
		w.Stop()
		b.mu.Lock()
		delay := b.backoff[key].delay
		b.mu.Unlock()
		if want := sharedRetryMin << i; delay != want {
			t.Fatalf("attempt %d: backoff = %v, want %v: an open that fails at once is not recovery", i, delay, want)
		}
		now = now.Add(delay)
	}

	healthy = true
	w, err := b.Watch(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	if ev := next(t, w); ev.Type != WatchBookmark || !ev.InitialEventsEnd {
		t.Fatalf("got %+v, want the snapshot boundary", ev)
	}
	backingOff := func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		_, ok := b.backoff[key]
		return ok
	}
	// A snapshot alone is not recovery: a watch that fails straight after it has not recovered.
	if !backingOff() {
		t.Fatal("the backoff was reset by a snapshot the watch did not outlive")
	}
	now = now.Add(minUsefulCycle)
	w.Stop() // the last subscriber leaves a watch that was of use
	if backingOff() {
		t.Error("the backoff survived a watch that was of use")
	}
}

// A watch can end after it was replaced: its pump notices last. That late end must not touch the
// replacement's backoff, or the next subscriber would reopen the upstream at once.
func TestAnObsoleteWatchsLateEndCannotEraseANewerBackoff(t *testing.T) {
	opens := 0
	failing := false
	up := backendFunc(func() (Watcher, error) {
		opens++
		if failing {
			return nil, UpstreamUnavailable("the API server is unavailable", 0)
		}
		return &stubWatcher{}, nil // opens, then idles: its end is replayed by hand below
	})
	b := NewSharedBackend(up, SharedOptions{})
	now := time.Unix(0, 0)
	b.now = func() time.Time { return now }
	scope := Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}
	key := scopeKey(scope)

	// 1. The old watch is open.
	w, err := b.Watch(t.Context(), scope)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	old := b.scopes[key]
	b.mu.Unlock()

	// 2. Its last subscriber leaves, so it is forgotten.
	w.Stop()

	// 3. A replacement fails to open and records a backoff.
	failing = true
	if _, err := b.Watch(t.Context(), scope); err == nil {
		t.Fatal("the replacement opened")
	}

	// 4. The old watch's end arrives late, looking like an early end of its own.
	old.die(ErrWatchClosed)

	if _, err := b.Watch(t.Context(), scope); err == nil {
		t.Fatal("a backoff-limited Watch succeeded")
	}
	if opens != 2 {
		t.Errorf("upstream opens = %d, want 2: the obsolete watch's end changed the newer backoff", opens)
	}
}

// The stream loop stops each connection after two early cycles, but a cohort of reconnecting
// subscribers would still reopen a shared upstream once per subscriber. The shared scope applies the
// same rule to its upstream watch: one early end is tolerated, the second backs off, and the cohort
// meets that backoff instead of the API server.
func TestASharedUpstreamThatKeepsEndingEarlyIsBackedOffForTheWholeCohort(t *testing.T) {
	for name, watcher := range map[string]func() Watcher{
		"closed before its snapshot": func() Watcher { return &endsWith{err: ErrWatchClosed} },
		"410 right after its snapshot": func() Watcher {
			return &endsWith{events: []WatchEvent{boundary}, err: ResyncRequired("the upstream resourceVersion expired (410 Gone)")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var opens atomic.Int32
			b := NewSharedBackend(backendFunc(func() (Watcher, error) { opens.Add(1); return watcher(), nil }), SharedOptions{})
			frozen := time.Unix(0, 0)
			b.now = func() time.Time { return frozen } // the backoff never expires during the test
			gw := &Gateway{StreamConfig: StreamConfig{Authorizer: AllowAll{}, Clients: func(context.Context, string, Principal) (Backend, error) { return b, nil }}}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			for range 100 {
				wg.Go(func() {
					err := gw.Stream(ctx, nil, Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}, "", &lockedSink{})
					var se *StreamError
					if !errors.As(err, &se) || se.Code != CodeUpstreamUnavailable || se.Terminal {
						t.Errorf("Stream() = %v, want a non-terminal UPSTREAM_UNAVAILABLE", err)
					}
				})
			}
			wg.Wait()
			if ctx.Err() != nil {
				t.Fatal("the streams did not end")
			}
			if n := opens.Load(); n != 2 {
				t.Errorf("upstream opens = %d for 100 streams, want 2: one tolerated early end, then the backoff", n)
			}
		})
	}
}

// Ordinary closures of a watch that was of use (the API server's routine timeout) are recovered in
// band, as often as they come, and never back off.
func TestASharedUpstreamsLongLivedWatchesRecoverWithoutBackoff(t *testing.T) {
	var opens atomic.Int32
	b := NewSharedBackend(backendFunc(func() (Watcher, error) {
		if opens.Add(1) > 5 {
			return nil, Forbidden("stop") // ends the test
		}
		return &endsWith{events: []WatchEvent{boundary}, err: ErrWatchClosed}, nil
	}), SharedOptions{})
	clock := steppingClock(2 * time.Second) // every watch outlives its snapshot by a second or more
	b.now = clock
	gw := &Gateway{now: clock, StreamConfig: StreamConfig{Authorizer: AllowAll{}, Clients: func(context.Context, string, Principal) (Backend, error) { return b, nil }}}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := gw.Stream(ctx, nil, Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}, "", &lockedSink{})
	var se *StreamError
	if !errors.As(err, &se) || se.Code != CodeForbidden {
		t.Fatalf("Stream() = %v after %d opens, want the scripted FORBIDDEN: long-lived watches were backed off", err, opens.Load())
	}
}

type lockedSink struct {
	mu     sync.Mutex
	events []Event
}

func (s *lockedSink) Emit(_ context.Context, ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	return nil
}

// A typed error that ends the shared upstream reaches every subscriber as it is, even when its Cause
// is ErrWatchClosed. StreamError.Unwrap exposes that cause, and reading it as a routine close would
// turn a terminal refusal into a resync the browser retries, and drop an unavailable error's hint.
func TestSharedSubscribersReceiveTypedErrorsCausedByAClosedWatch(t *testing.T) {
	forbidden := Forbidden("the gateway's identity may no longer watch this scope")
	forbidden.Cause = ErrWatchClosed
	unavailable := UpstreamUnavailable("the API server is restarting", 4*time.Second)
	unavailable.Cause = ErrWatchClosed
	for name, sent := range map[string]*StreamError{"terminal FORBIDDEN": forbidden, "UPSTREAM_UNAVAILABLE with a hint": unavailable} {
		for _, asEvent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/event=%v", name, asEvent), func(t *testing.T) {
				b := NewSharedBackend(backendFunc(func() (Watcher, error) {
					if asEvent { // a WatchError event on the open watch
						return &endsWith{events: []WatchEvent{boundary, {Type: WatchError, Err: sent}}, err: ErrWatchClosed}, nil
					}
					return &endsWith{events: []WatchEvent{boundary}, err: sent}, nil // a failing Next
				}), SharedOptions{})
				w, err := b.Watch(t.Context(), Scope{Version: "v1", Resource: "configmaps", Namespace: "app"})
				if err != nil {
					t.Fatal(err)
				}
				defer w.Stop()
				for err == nil {
					var ev WatchEvent
					ev, err = w.Next(t.Context())
					if err == nil && ev.Type == WatchError {
						err = ev.Err
					}
				}
				var got *StreamError
				if !errors.As(err, &got) {
					t.Fatalf("subscriber got %v, want the typed error", err)
				}
				if got.Code != sent.Code || got.Terminal != sent.Terminal || got.Message != sent.Message || !equalHint(got.RetryAfterMs, sent.RetryAfterMs) {
					t.Errorf("subscriber got %s terminal=%v %q hint=%v, want %s terminal=%v %q hint=%v",
						got.Code, got.Terminal, got.Message, got.RetryAfterMs, sent.Code, sent.Terminal, sent.Message, sent.RetryAfterMs)
				}
			})
		}
	}
}

func equalHint(a, b *int) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// Seen from the browser: the terminal refusal is the stream's last event, not a resync and a retry.
func TestAStreamOverASharedBackendEndsWithTheTypedRefusal(t *testing.T) {
	forbidden := Forbidden("the gateway's identity may no longer watch this scope")
	forbidden.Cause = ErrWatchClosed
	b := NewSharedBackend(backendFunc(func() (Watcher, error) {
		return &endsWith{events: []WatchEvent{boundary}, err: forbidden}, nil
	}), SharedOptions{})
	sink, err := streamOver(t, b, nil)
	var se *StreamError
	if !errors.As(err, &se) || se.Code != CodeForbidden || !se.Terminal {
		t.Fatalf("Stream() = %v, want the terminal FORBIDDEN", err)
	}
	var codes []string
	for _, ev := range sink.events {
		if ev.Type == EventError {
			codes = append(codes, fmt.Sprintf("%s terminal=%v", ev.Code, ev.Terminal))
		}
	}
	if len(codes) != 1 || codes[0] != "FORBIDDEN terminal=true" {
		t.Errorf("error events = %v, want only the terminal FORBIDDEN", codes)
	}
}
