package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A namespace with more objects than one subscriber's queue is deep. 300 ConfigMaps is a small
// namespace; sharedQueueDepth is 256.

// countSnapshot reads added* to the boundary bookmark, returning the count or the error that ended
// the queue. Unlike drainSnapshot it does not t.Fatal, because the whole question here is WHICH
// error comes back.
func countSnapshot(w Watcher) (int, error) {
	n := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ev, err := w.Next(ctx)
		cancel()
		if err != nil {
			return n, err
		}
		switch ev.Type {
		case WatchAdded:
			n++
		case WatchBookmark:
			if ev.InitialEventsEnd {
				return n, nil
			}
		case WatchError:
			return n, ev.Err
		}
	}
}

func TestSharedBackendJoinerGetsSnapshotOfLargeScope(t *testing.T) {
	const n = 300

	up := newFakeUpstream()
	b := NewSharedBackend(up)

	first, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer first.Stop()

	go func() {
		for i := range n {
			up.send(WatchEvent{Type: WatchAdded, Object: obj(fmt.Sprintf("uid-%d", i), fmt.Sprintf("cm-%d", i), "10")})
		}
		up.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	}()

	if got, err := countSnapshot(first); err != nil || got != n {
		t.Fatalf("first subscriber: %d objects, err=%v; want %d, nil", got, err, n)
	}

	// Now a second tab joins the warm cache. This is the path EVERY joiner takes, and nobody is
	// draining its queue while subscribe() fills it.
	joiner, err := b.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatalf("joiner Watch: %v", err)
	}
	defer joiner.Stop()

	got, err := countSnapshot(joiner)
	if err != nil || got != n {
		t.Fatalf("joiner's snapshot from the warm cache: %d objects, err=%v; want %d, nil", got, err, n)
	}
}
