package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Fan-out: one upstream watch per SCOPE, not per browser tab.
//
// Without this, ten tabs on the same namespace are ten watches on the API server, ten snapshots, and
// ten copies of the same object graph — and a reconnect storm (a laptop lid closing on a floor of
// them) multiplies it. With it, they are one watch and one warm cache, and a joining subscriber gets
// its reset…synced from that cache without touching the API server at all.
//
// # It is OPT-IN, and here is the thing you are opting into
//
// A shared watch can only be opened ONCE, so it can only be opened as ONE identity. That is the
// whole trade, and it must be stared at rather than glossed:
//
//   - WITHOUT sharing, ClientFor hands the gateway a client acting AS the caller. Kubernetes' own
//     RBAC is then the enforcement: if the caller may not watch Secrets, the API server says so, and
//     no bug in this library can change that. Authorization is defence in depth.
//   - WITH sharing, the upstream watch runs as ONE identity — your service account — and every
//     subscriber reads from its cache. **Your Authorizer becomes the only thing standing between a
//     caller and the objects.** A bug there is not a bug, it is a disclosure.
//
// So SharedBackend is not the default and never will be. A host opts in by wiring it deliberately —
// and there is a way to opt in WITHOUT giving up the boundary, which is the wiring you want:
//
//	shared := gateway.NewSharedBackend(myServiceAccountBackend)    // one watch, ONE identity…
//	opts.Authorizer = kube.SSARAuthorizer(clientset, subjectOf)    // …but Kubernetes still decides
//	opts.Clients = func(string, gateway.Principal) (gateway.Backend, error) { return shared, nil }
//
// kube.SSARAuthorizer asks the API server, with a SubjectAccessReview, whether THIS user may list and
// watch THIS resource here — before the subscriber is served from the shared cache. RBAC is the
// boundary again, and the sharing costs you nothing but a round-trip per snapshot cycle. See
// docs/auth.md.
//
// The library cannot make that choice for you, because it is a choice about YOUR threat model. What
// it can do is refuse to make it silently, which is what this comment is for.

// sharedQueueDepth is how many live events one subscriber may fall behind by before the gateway gives
// up on catching it up and resnapshots it instead.
//
// A bounded queue is not a limitation here, it is the design. The alternative — an unbounded one —
// turns a single slow consumer (a backgrounded tab, a paused debugger) into unbounded memory growth
// in a process serving everyone else. When a subscriber overflows, it is handed a continuity-loss
// error, which the stream loop already knows how to recover from: a fresh reset…synced, served from
// the warm cache, costing the API server nothing. Slowness degrades into a resnapshot, never into a
// leak and never into a lie.
const sharedQueueDepth = 256

// SharedBackend multiplexes many consumers onto one upstream watch per scope.
//
// It IS a Backend, so it drops in behind the same seam as any other — the stream loop cannot tell the
// difference, which is exactly what "the protocol is backend-agnostic" has to mean if it means
// anything (spec §5, §6).
type SharedBackend struct {
	upstream Backend

	mu     sync.Mutex
	scopes map[string]*sharedScope
}

// NewSharedBackend shares one upstream watch per scope across every consumer of it.
//
// Read the package comment above about identity before you wire this in: the upstream is opened once,
// as whatever identity `upstream` carries, and your Authorizer becomes the security boundary.
func NewSharedBackend(upstream Backend) *SharedBackend {
	return &SharedBackend{upstream: upstream, scopes: map[string]*sharedScope{}}
}

var _ Backend = (*SharedBackend)(nil)

// Watch joins the shared watch for this scope, opening it if this is the first subscriber.
//
// The caller's context is deliberately IGNORED, and that is the one surprising line in this file. It
// is the context of ONE browser's request; the upstream watch belongs to ALL of them. Honouring it
// here would mean the whole shared watch — and everyone else's stream — dies the moment whichever
// tab happened to open it goes away. A subscriber's own lifetime is bounded by Stop() instead, and
// the last one out cancels the upstream (see leave).
func (b *SharedBackend) Watch(_ context.Context, scope Scope) (Watcher, error) {
	key := scopeKey(scope)

	b.mu.Lock()
	s, ok := b.scopes[key]
	if !ok {
		var err error
		s, err = b.startScope(scope, key)
		if err != nil {
			b.mu.Unlock()
			return nil, err
		}
		b.scopes[key] = s
	}
	b.mu.Unlock()

	return s.subscribe()
}

// startScope opens the one upstream watch for a scope and pumps it. Called with b.mu held.
//
// The upstream watch gets its OWN context, deliberately detached from the subscriber whose arrival
// happened to open it. Tying the shared watch to one browser's request context would mean that when
// that particular tab closes, everyone else's stream dies with it — a bug that would be invisible
// with one subscriber and baffling with two.
func (b *SharedBackend) startScope(scope Scope, key string) (*sharedScope, error) {
	ctx, cancel := context.WithCancel(context.Background())

	w, err := b.upstream.Watch(ctx, scope)
	if err != nil {
		cancel()
		return nil, err
	}

	s := &sharedScope{
		backend: b,
		key:     key,
		watcher: w,
		cancel:  cancel,
		cache:   map[string]KRMObject{},
		subs:    map[*subscriber]struct{}{},
	}
	go s.pump(ctx)
	return s, nil
}

// forget drops a dead-or-empty scope so the next Watch opens a fresh one.
func (b *SharedBackend) forget(key string, s *sharedScope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.scopes[key] == s { // not a newer incarnation
		delete(b.scopes, key)
	}
}

// sharedScope is one upstream watch, its warm cache, and everyone reading from it.
type sharedScope struct {
	backend *SharedBackend
	key     string
	watcher Watcher
	cancel  context.CancelFunc

	mu     sync.Mutex
	cache  map[string]KRMObject // uid -> the object's current state
	synced bool                 // has the upstream snapshot completed at least once?
	dead   bool
	subs   map[*subscriber]struct{}
}

// subscriber is one consumer's view of the shared watch: a SNAPSHOT, then a bounded queue of live
// events.
//
// # Why two queues, and not one
//
// The first version put both through the bounded channel, and it was broken in a way that only a
// realistic scope reveals: a namespace with more objects than the queue is deep (300 ConfigMaps;
// sharedQueueDepth is 256) could not be served AT ALL. subscribe() fills the queue from the warm
// cache while nobody is draining it yet, the 257th object overflows, the subscriber is told it "fell
// behind" — and the stream loop's recovery for that is to resnapshot, which does the same thing
// again. **An infinite resync loop, on a small namespace.** Both of my own tests used two objects.
//
// The two things were never the same, and conflating them was the bug:
//
//   - the SNAPSHOT is the consumer's STARTING STATE. It is finite, known in advance, and dropping any
//     of it is not backpressure — it is a wrong answer, and the recovery for a wrong answer is to send
//     it again, forever. It gets an unbounded slice, drained first.
//   - LIVE EVENTS are open-ended. A consumer that cannot keep up with them genuinely is falling
//     behind, and resnapshotting it from the warm cache is exactly right. They keep the bounded
//     channel, and the backpressure it exists for.
type subscriber struct {
	// pending is the snapshot: added* and the boundary bookmark. Guarded by sharedScope.mu, and
	// drained by Next() before a single live event is read.
	pending []WatchEvent
	// ready wakes a reader that is already blocked on `ch` when the snapshot lands in `pending`.
	//
	// It is not decoration. The snapshot no longer travels through `ch`, so a consumer that called
	// Next() before the upstream finished its first cycle parks on a channel that will never carry
	// the thing it is waiting for — and waits forever. (It did. Every test that subscribed before the
	// boundary bookmark hung for exactly the timeout.) Buffered 1: a missed signal is impossible,
	// because a reader re-checks `pending` before parking again.
	ready chan struct{}
	ch    chan WatchEvent
	// awaiting is true until this subscriber has been handed its snapshot. Live events are not
	// delivered to it before then — it will receive the cache instead, which ALREADY contains them.
	// Forwarding both would deliver an object twice, and the second copy could be older.
	awaiting bool
	closed   bool
	// reason is why this subscriber's queue ended, and it lives BESIDE the queue rather than in it.
	//
	// The first version of this pushed the error INTO the channel — which cannot work, because the
	// case where we need to send it is precisely the case where the channel is full. The reason was
	// silently dropped and the consumer saw a bare close: it still recovered (a closed watch means
	// resnapshot), but nothing anywhere could say WHY, which is the difference between a system you
	// can operate and one you can only restart. Written before close(ch); a receiver that observes
	// the close is guaranteed to see it.
	reason error
}

// end closes a subscriber's queue with a reason. Idempotent.
func (sub *subscriber) end(reason error) {
	if sub.closed {
		return
	}
	sub.closed = true
	sub.reason = reason
	close(sub.ch)
}

func (s *sharedScope) subscribe() (Watcher, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.dead {
		// It died between the map lookup and here. Say "reopen" rather than inventing an error: the
		// stream loop will begin a new cycle, and that cycle opens a fresh shared scope.
		return nil, ErrWatchClosed
	}

	sub := &subscriber{
		ch:       make(chan WatchEvent, sharedQueueDepth),
		ready:    make(chan struct{}, 1),
		awaiting: true,
	}
	s.subs[sub] = struct{}{}

	// The warm cache, and the entire point of the exercise: if the upstream snapshot is already
	// complete, this consumer gets its whole reset…synced now, from memory, and the API server never
	// hears about it.
	if s.synced {
		s.deliverSnapshotLocked(sub)
	}

	return &sharedWatcher{scope: s, sub: sub}, nil
}

// deliverSnapshotLocked hands one subscriber the whole cache as a snapshot, terminated by the
// boundary bookmark. Called with s.mu held, which is what keeps it atomic with respect to live
// events: no event can slip between the snapshot and the bookmark.
//
// It CANNOT overflow, and that is the point. The snapshot is the consumer's starting state, not a
// backlog — see the subscriber comment. Its size is the size of the scope, which the operator chose
// when they allowlisted it; a 5000-object namespace costs a 5000-element slice, once, per joiner.
func (s *sharedScope) deliverSnapshotLocked(sub *subscriber) {
	if sub.closed {
		return
	}
	sub.pending = make([]WatchEvent, 0, len(s.cache)+1)
	for _, obj := range s.cache {
		sub.pending = append(sub.pending, WatchEvent{Type: WatchAdded, Object: obj})
	}
	sub.pending = append(sub.pending, WatchEvent{Type: WatchBookmark, InitialEventsEnd: true})
	sub.awaiting = false

	// Wake a reader that is already parked on `ch` waiting for a snapshot that will never arrive
	// there. Non-blocking: the buffer holds the one signal that matters.
	select {
	case sub.ready <- struct{}{}:
	default:
	}
}

// offer enqueues without blocking. A full queue means this consumer cannot keep up, and the honest
// response is to stop trying: hand it a continuity-loss error, which the stream loop recovers from
// with a fresh snapshot off the warm cache. Blocking here would let one stalled browser stall the
// pump, and with it every other subscriber to this scope.
func (sub *subscriber) offer(ev WatchEvent) bool {
	if sub.closed {
		return false
	}
	select {
	case sub.ch <- ev:
		return true
	default:
		sub.end(ResyncRequired(
			"this consumer fell too far behind the shared watch; resnapshotting from the warm cache"))
		return false
	}
}

// pump is the single reader of the upstream watch. One goroutine per scope, for the life of the
// upstream watch, and it is the only thing that ever touches the cache.
func (s *sharedScope) pump(ctx context.Context) {
	defer s.watcher.Stop()

	for {
		ev, err := s.watcher.Next(ctx)
		if err != nil {
			// Upstream ended — cleanly (reopen), or with a 410, or the context went away. Every one of
			// those means the same thing to a subscriber: continuity is lost, start a new cycle. The
			// scope dies here; the next Watch builds a fresh one, and N subscribers resyncing at once
			// still produce exactly ONE new upstream watch.
			s.die(err)
			return
		}

		s.mu.Lock()
		switch ev.Type {
		case WatchBookmark:
			if ev.InitialEventsEnd {
				// The upstream snapshot is complete: the cache is now a true picture of the scope, and
				// everyone waiting for one can have it.
				s.synced = true
				for sub := range s.subs {
					if sub.awaiting {
						s.deliverSnapshotLocked(sub)
					}
				}
			}
			// Routine bookmarks are absorbed here and never fanned out. They carry no object a
			// consumer wants, and the boundary each subscriber sees is the one WE synthesize.

		case WatchAdded, WatchModified:
			// The same partial-object guard the stream loop applies (spec §2) — and it MUST be applied
			// here too, one layer lower, because this cache is REPLAYED to every future joiner. A husk
			// forwarded once blanks one consumer's object; a husk CACHED is a husk served to everyone
			// who arrives later, for as long as the scope lives.
			if reason := partialReason(ev.Object); reason != "" {
				s.mu.Unlock()
				s.die(ResyncRequired(reason))
				return
			}
			if uid := ev.Object.UID(); uid != "" {
				s.cache[uid] = ev.Object
			}
			s.fanOutLocked(ev)

		case WatchDeleted:
			id := identityOf(ev.Object)
			if id == nil {
				// A degenerate tombstone: we cannot know WHICH object left. Guessing would evict the
				// wrong one from a cache that is then served to everybody.
				s.mu.Unlock()
				s.die(ResyncRequired("deletion tombstone carried no trustworthy uid"))
				return
			}
			delete(s.cache, id.UID)
			s.fanOutLocked(ev)

		case WatchError:
			s.mu.Unlock()
			s.die(ev.Err)
			return
		}
		s.mu.Unlock()
	}
}

// fanOutLocked delivers one live event to every subscriber that has had its snapshot. Called with
// s.mu held.
func (s *sharedScope) fanOutLocked(ev WatchEvent) {
	for sub := range s.subs {
		if sub.awaiting {
			// It has not been handed the cache yet, and the cache already contains this event's
			// effect. Sending it as well would deliver the object twice.
			continue
		}
		sub.offer(ev)
	}
}

// die ends the scope: every subscriber is told continuity was lost, and the scope is removed so the
// next Watch opens a fresh upstream.
func (s *sharedScope) die(cause error) {
	s.backend.forget(s.key, s)
	s.cancel()

	// Whatever ended the upstream — a clean close, a 410, a cancelled context — means one thing to a
	// subscriber: continuity is lost, start a new cycle. Say it as a non-terminal RESYNC_REQUIRED so
	// the stream loop announces it and resnapshots, rather than tearing the browser's connection down.
	if cause == nil || errors.Is(cause, ErrWatchClosed) {
		cause = ResyncRequired("the shared upstream watch ended; a new snapshot cycle follows")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = true
	for sub := range s.subs {
		// The event first (best-effort: a full queue means this consumer is already being resynced for
		// having fallen behind), then the close, which carries the reason regardless.
		sub.offer(WatchEvent{Type: WatchError, Err: cause})
		sub.end(cause)
	}
	s.subs = map[*subscriber]struct{}{}
}

// leave removes one subscriber, and tears the whole scope down when the last one goes. Nobody is
// watching, so nothing should be watched: a shared watch that outlives its audience is a leak with
// a cache attached.
func (s *sharedScope) leave(sub *subscriber) {
	s.mu.Lock()
	delete(s.subs, sub)
	sub.end(nil) // its own reader is gone; nothing left to tell it
	empty := len(s.subs) == 0 && !s.dead
	if empty {
		s.dead = true
	}
	s.mu.Unlock()

	if empty {
		s.backend.forget(s.key, s)
		s.cancel() // stops pump, which stops the upstream watch
	}
}

// sharedWatcher is one consumer's Watcher over the shared scope: a pull face on a pushed queue.
type sharedWatcher struct {
	scope *sharedScope
	sub   *subscriber
	once  sync.Once
}

// nextPending pops one snapshot event, if any remain. `pending` is written under the scope's lock, so
// it is read under it too — the critical section is a slice index, and it is not held across a block.
func (w *sharedWatcher) nextPending() (WatchEvent, bool) {
	w.scope.mu.Lock()
	defer w.scope.mu.Unlock()

	if len(w.sub.pending) == 0 {
		return WatchEvent{}, false
	}
	ev := w.sub.pending[0]
	w.sub.pending = w.sub.pending[1:]
	return ev, true
}

func (w *sharedWatcher) Next(ctx context.Context) (WatchEvent, error) {
	for {
		// The snapshot first, to exhaustion, before a single live event. It is the consumer's starting
		// state, and it is not allowed to be dropped, truncated or interleaved.
		if ev, ok := w.nextPending(); ok {
			return ev, nil
		}

		select {
		case <-ctx.Done():
			return WatchEvent{}, ctx.Err()
		case <-w.sub.ready:
			continue // the snapshot landed in `pending`; go read it
		case ev, ok := <-w.sub.ch:
			if !ok {
				// Drained, and closed. `reason` was written before the close, so observing the close
				// guarantees we see it — and it is what turns "your stream restarted" into "your stream
				// restarted BECAUSE you fell behind", which is the only version anyone can act on.
				if w.sub.reason != nil {
					return WatchEvent{}, w.sub.reason
				}
				return WatchEvent{}, ErrWatchClosed
			}
			return ev, nil
		}
	}
}

func (w *sharedWatcher) Stop() {
	w.once.Do(func() { w.scope.leave(w.sub) })
}

// scopeKey is the identity of a shared watch: two consumers share an upstream exactly when they are
// asking the same question of the same cluster.
//
// Every field participates, including the label selector — a selector changes WHICH objects are in
// the snapshot, so two selectors are two scopes, and merging them would hand a consumer objects it
// did not ask for. `\x1f` (unit separator) joins them: it cannot occur in any of these values, so no
// combination of legal fields can be made to collide with another.
func scopeKey(s Scope) string {
	return strings.Join([]string{
		s.Target, s.Group, s.Version, s.Resource, s.Namespace, s.Name, s.LabelSelector,
	}, "\x1f")
}
