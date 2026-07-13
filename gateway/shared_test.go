package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	b := NewSharedBackend(up)

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
	b := NewSharedBackend(up)

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
	b := NewSharedBackend(up)

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

// A partial object must never enter the cache. The stream loop guards its own output, but this cache
// is REPLAYED to every future joiner: a husk forwarded once blanks one consumer's object; a husk
// CACHED is served to everybody who arrives later, for as long as the scope lives.
func TestAPartialObjectPoisonsNothing(t *testing.T) {
	up := newFakeUpstream()
	b := NewSharedBackend(up)

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
	b := NewSharedBackend(up)

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
	b := NewSharedBackend(up)

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
