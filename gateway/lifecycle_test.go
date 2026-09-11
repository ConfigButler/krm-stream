package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type lifecycleCounter struct {
	mu       sync.Mutex
	counts   map[ObservationKind]int
	negative bool
}

func (c *lifecycleCounter) Observe(o Observation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = make(map[ObservationKind]int)
	}
	c.counts[o.Kind]++
	if c.counts[ObservationSharedSubscriptionClosed] > c.counts[ObservationSharedSubscriptionOpened] || c.counts[ObservationStreamClosed] > c.counts[ObservationStreamOpened] {
		c.negative = true
	}
}
func (c *lifecycleCounter) assert(t *testing.T, open, closed int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.negative || c.counts[ObservationSharedSubscriptionOpened] != open || c.counts[ObservationSharedSubscriptionClosed] != closed {
		t.Fatalf("lifetimes = %v, negative=%v", c.counts, c.negative)
	}
}

func TestSharedSubscriptionLifecycle(t *testing.T) {
	b := newFakeUpstream()
	counter := &lifecycleCounter{}
	shared := NewSharedBackendWithOptions(b, SharedOptions{QueueDepth: 1, Observer: counter})
	first, err := shared.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatal(err)
	}
	b.send(WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	drainSnapshot(t, first)
	var wg sync.WaitGroup
	const joins = 20
	for range joins {
		wg.Go(func() {
			w, err := shared.Watch(t.Context(), sharedScopeUnderTest)
			if err != nil {
				t.Error(err)
				return
			}
			w.Stop()
			w.Stop()
		})
	}
	wg.Wait()
	counter.assert(t, joins+1, joins)
	if b.opens() != 1 {
		t.Fatal("warm joins reopened upstream")
	}
	// Overflow ends the attachment before the owner calls Stop.
	sw := first.(*sharedWatcher)
	sw.scope.mu.Lock()
	sw.sub.offer(WatchEvent{Type: WatchAdded, Object: obj("a", "a", "1")})
	sw.sub.offer(WatchEvent{Type: WatchAdded, Object: obj("b", "b", "2")})
	sw.scope.mu.Unlock()
	counter.assert(t, joins+1, joins+1)
	first.Stop()
	first.Stop()
	counter.assert(t, joins+1, joins+1)
}

func TestScopeDeathAndFailedAttachmentLifetimes(t *testing.T) {
	counter := &lifecycleCounter{}
	shared := NewSharedBackendWithOptions(newFakeUpstream(), SharedOptions{Observer: counter})
	w, err := shared.Watch(t.Context(), sharedScopeUnderTest)
	if err != nil {
		t.Fatal(err)
	}
	s := w.(*sharedWatcher).scope
	s.die(ErrWatchClosed)
	counter.assert(t, 1, 1)
	if _, err := s.subscribe(); !errors.Is(err, ErrWatchClosed) {
		t.Fatal(err)
	}
	w.Stop()
	s.die(ErrWatchClosed)
	counter.assert(t, 1, 1)
}

type failedOpenBackend struct{}

func (failedOpenBackend) Watch(context.Context, Scope) (Watcher, error) {
	return nil, errors.New("open failed")
}
func TestFailedSharedOpenHasNoLifetime(t *testing.T) {
	counter := &lifecycleCounter{}
	b := NewSharedBackendWithOptions(failedOpenBackend{}, SharedOptions{Observer: counter})
	if _, err := b.Watch(t.Context(), sharedScopeUnderTest); err == nil {
		t.Fatal("expected failure")
	}
	counter.assert(t, 0, 0)
}
