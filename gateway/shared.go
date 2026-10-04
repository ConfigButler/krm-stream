package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
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
//	opts.Authorizer = kube.SubjectAccessReviewAuthorizer(clientset, subjectOf)    // …but Kubernetes still decides
//	opts.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return shared, nil }
//
// kube.SubjectAccessReviewAuthorizer asks the API server, with a SubjectAccessReview, whether THIS user may list and
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

// The backoff after a shared upstream watch fails: opening it fails with UPSTREAM_UNAVAILABLE, the open
// watch dies of it, or the watch keeps ending before it is of use. Its subscribers each reconnect on
// their own budget, so without it the API server would see one attempt per subscriber per retry. With
// it, a scope sees at most one attempt per backoff period however many subscribers are waiting, and
// the rest are told when to come back.
//
// A watch is of use once it has completed its snapshot and stayed live for minUsefulCycle, the same
// rule the stream loop applies to one connection. Only such a watch's end resets the backoff; one
// that opens and fails at once has not recovered. One early end is tolerated, as continuity loss is
// (§5); a second in a row backs off.
const (
	sharedRetryMin = time.Second
	sharedRetryMax = 30 * time.Second
)

// SharedOptions configures an opt-in SharedBackend. Zero values preserve the bounded, safe defaults.
type SharedOptions struct {
	// QueueDepth is the maximum number of live events a subscriber may lag before it is resnapshotted.
	// Zero uses 256.
	QueueDepth int
	// Observer receives subscription-lifecycle and overflow signals. It must not block.
	Observer Observer
}

// SharedBackend multiplexes many consumers onto one upstream watch per scope.
//
// It IS a Backend, so it drops in behind the same seam as any other — the stream loop cannot tell the
// difference, which is exactly what "the protocol is backend-agnostic" has to mean if it means
// anything (spec §5, §6).
type SharedBackend struct {
	upstream   Backend
	queueDepth int
	observer   Observer

	mu       sync.Mutex
	scopes   map[string]*sharedScope
	openings map[string]*sharedOpening
	backoff  map[string]*sharedBackoff
	now      func() time.Time
}

// sharedOpening is one scope's upstream watch while it is being opened: registered under the
// backend's lock, opened without it, and settled under it again.
//
// It exists because opening is a network round trip of unknown length, and the backend's lock is
// taken by every scope. Holding the lock across it let one slow API server hold up every unrelated
// namespace, and gave a caller nothing to abandon: its own context was not the opening's, and nothing
// else could cancel it. Now the lock covers only the bookkeeping, and an opening belongs to whoever
// is waiting for it, counted, so the last one to leave cancels it.
type sharedOpening struct {
	key   string
	scope Scope
	// ctx is the upstream watch's own context: cancelled when the last waiter abandons the opening,
	// and otherwise handed on to the scope it becomes, whose last subscriber cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	// done is closed, under the backend's lock, once the opening has settled for its waiters.
	done chan struct{}

	// Guarded by the backend's lock.
	//
	// waiters counts the callers still waiting. It changes only before done is closed; after that,
	// every waiter takes one of subs (or err) on its way out, however it leaves.
	waiters int
	// subs holds one subscription per waiter when the opening succeeds, attached to the new scope
	// at the moment it is published. A waiter therefore never has to subscribe after the fact, and
	// there is no instant at which the scope holds an upstream watch and nobody who will release it.
	subs []*subscriber
	// err is the opening's failure, shared by every waiter.
	err error
}

// sharedBackoff is one scope's failed attempts to open its upstream watch.
type sharedBackoff struct {
	until time.Time
	delay time.Duration
	// early counts consecutive watches that ended before they were of use.
	early int
}

// NewSharedBackend shares one upstream watch per scope across every consumer of it.
//
// Read the package comment above about identity before you wire this in: the upstream is opened once,
// as whatever identity `upstream` carries, and your Authorizer becomes the security boundary.
func NewSharedBackend(upstream Backend) *SharedBackend {
	return NewSharedBackendWithOptions(upstream, SharedOptions{})
}

// NewSharedBackendWithOptions shares one upstream watch per scope with explicit operational limits.
func NewSharedBackendWithOptions(upstream Backend, options SharedOptions) *SharedBackend {
	depth := options.QueueDepth
	if depth < 1 {
		depth = sharedQueueDepth
	}
	return &SharedBackend{
		upstream:   upstream,
		queueDepth: depth,
		observer:   options.Observer,
		scopes:     map[string]*sharedScope{},
		openings:   map[string]*sharedOpening{},
		backoff:    map[string]*sharedBackoff{},
		now:        time.Now,
	}
}

func (b *SharedBackend) observe(observation Observation) {
	if b.observer != nil {
		b.observer.Observe(observation)
	}
}

var _ Backend = (*SharedBackend)(nil)

// Watch joins the shared watch for this scope, opening it if this is the first subscriber.
//
// The caller's context bounds only the caller's WAIT, never the upstream watch. It is the context of
// ONE browser's request; the upstream watch belongs to ALL of them. Handing it to the upstream would
// mean the whole shared watch — and everyone else's stream — dies the moment whichever tab happened to
// open it goes away. So the upstream gets a context of its own, owned by the scope:
//
//   - while it is opening, by everyone waiting for it. Each may give up through its own context, and
//     returns at once; the last to give up cancels the opening.
//   - once it is open, by its subscribers. Each one's lifetime is bounded by Stop(), and the last one
//     out cancels the upstream (see leave).
//
// The backend's lock is never held across the upstream's Watch, so a scope that is slow to open holds
// up only the callers waiting for that scope.
func (b *SharedBackend) Watch(ctx context.Context, scope Scope) (Watcher, error) {
	// A caller that has already gone must not start, or join, an opening it would at once abandon.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := scopeKey(scope)

	b.mu.Lock()
	if s, ok := b.scopes[key]; ok {
		b.mu.Unlock()
		return s.subscribe()
	}
	op, ok := b.openings[key]
	if !ok {
		if wait := b.backoffRemainingLocked(key); wait > 0 {
			b.mu.Unlock()
			return nil, UpstreamUnavailable("the upstream is unavailable; retry later", wait)
		}
		op = b.beginOpeningLocked(key, scope)
	}
	op.waiters++
	b.mu.Unlock()

	select {
	case <-op.done:
		return b.collect(op)
	case <-ctx.Done():
		b.abandon(op)
		return nil, ctx.Err()
	}
}

// beginOpeningLocked registers a scope's opening and starts it. Called with b.mu held; the upstream
// call itself runs on its own goroutine, without the lock.
func (b *SharedBackend) beginOpeningLocked(key string, scope Scope) *sharedOpening {
	ctx, cancel := context.WithCancel(context.Background())
	op := &sharedOpening{key: key, scope: scope, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	b.openings[key] = op
	go func() {
		w, err := b.upstream.Watch(ctx, scope)
		b.settle(op, w, err)
	}()
	return op
}

// settle publishes an opening's result to its waiters, if it still has any.
//
// An opening every waiter abandoned is discarded whole: its watcher is stopped, and it touches
// neither the scope map, nor the backoff, nor whatever opening has since replaced it. Its failure, if
// it failed, is most likely the cancellation its waiters caused, and in any case nobody asked.
func (b *SharedBackend) settle(op *sharedOpening, w Watcher, err error) {
	b.mu.Lock()
	if b.openings[op.key] != op { // its last waiter left, and unregistered it
		b.mu.Unlock()
		op.cancel()
		if w != nil {
			w.Stop()
		}
		return
	}
	delete(b.openings, op.key)
	if err != nil {
		op.cancel()
		b.recordFailureLocked(op.key, err) // once, however many are waiting
		op.err = err
	} else {
		s := newSharedScope(b, op.key, op.scope, w, op.cancel)
		b.scopes[op.key] = s
		// Attach every waiter before the scope is visible to anyone else, so it is never without the
		// subscribers whose departure will cancel it.
		s.mu.Lock()
		for range op.waiters {
			op.subs = append(op.subs, s.attachLocked())
		}
		s.mu.Unlock()
		go s.pump(op.ctx)
	}
	close(op.done)
	b.mu.Unlock()
}

// collect hands a waiter its share of a settled opening: the failure, or one of the subscriptions
// attached on its behalf.
func (b *SharedBackend) collect(op *sharedOpening) (Watcher, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if op.err != nil {
		return nil, op.err
	}
	sub := op.subs[len(op.subs)-1]
	op.subs = op.subs[:len(op.subs)-1]
	return &sharedWatcher{scope: sub.scope, sub: sub}, nil
}

// abandon withdraws a waiter whose context ended. Before the opening settles, the waiter simply stops
// being counted, and the last one cancels the opening and unregisters it, so the next Watch starts
// afresh. After it settles — the two raced — the waiter still owns a subscription, and releases it
// exactly as a subscriber would.
func (b *SharedBackend) abandon(op *sharedOpening) {
	b.mu.Lock()
	select {
	case <-op.done: // settled first: done is closed under b.mu, so this is decided
		b.mu.Unlock()
		if w, err := b.collect(op); err == nil {
			w.Stop()
		}
		return
	default:
	}
	op.waiters--
	last := op.waiters == 0
	if last && b.openings[op.key] == op {
		delete(b.openings, op.key)
	}
	b.mu.Unlock()
	if last {
		op.cancel() // the upstream's Watch returns when it honours this; settle discards what it returns
	}
}

func (b *SharedBackend) backoffRemainingLocked(key string) time.Duration {
	if bo := b.backoff[key]; bo != nil {
		return bo.until.Sub(b.now())
	}
	return 0
}

// recordFailureLocked starts or doubles a scope's backoff after a retryable failure. Any other
// failure is not the upstream being away, and backing off would only delay the real answer; it
// leaves the backoff as it is, because it is not a recovery either.
func (b *SharedBackend) recordFailureLocked(key string, err error) {
	var se *StreamError
	if !errors.As(err, &se) || se == nil || se.Terminal || se.Code != CodeUpstreamUnavailable {
		return
	}
	now := b.now()
	for k, old := range b.backoff {
		if now.Sub(old.until) > sharedRetryMax { // quiet for a full period: forget it, so the map stays small
			delete(b.backoff, k)
		}
	}
	bo := b.backoff[key]
	switch {
	case bo == nil:
		bo = &sharedBackoff{delay: sharedRetryMin}
		b.backoff[key] = bo
	case bo.delay == 0: // so far only an early end, which waits for nothing
		bo.delay = sharedRetryMin
	default:
		bo.delay = min(2*bo.delay, sharedRetryMax)
	}
	wait := bo.delay
	if se.RetryAfterMs != nil {
		if hint := time.Duration(*se.RetryAfterMs) * time.Millisecond; hint > wait {
			wait = min(hint, sharedRetryMax)
		}
	}
	bo.until = now.Add(wait)
}

// newSharedScope wraps an opened upstream watch. Its context and cancel are the opening's: the
// watch's own, detached from whichever subscriber's arrival happened to open it. Tying the shared
// watch to one browser's request context would mean that when that particular tab closes, everyone
// else's stream dies with it — a bug that would be invisible with one subscriber and baffling with
// two.
func newSharedScope(b *SharedBackend, key string, scope Scope, w Watcher, cancel context.CancelFunc) *sharedScope {
	return &sharedScope{
		backend: b,
		key:     key,
		scope:   scope,
		watcher: w,
		cancel:  cancel,
		cache:   map[string]KRMObject{},
		subs:    map[*subscriber]struct{}{},
	}
}

// scopeEnd is how a shared upstream watch ended, for the backoff.
type scopeEnd struct {
	// useful: it completed its snapshot and stayed live for minUsefulCycle. Its end resets the backoff.
	useful bool
	// early: it lost continuity (a close, a 410) before it was of use.
	early bool
	// failure is an UPSTREAM_UNAVAILABLE it died of, if any.
	failure error
	cause   error
}

// forget drops a dead-or-empty scope so the next Watch opens a fresh one, and accounts for how it
// ended in the same critical section, so no Watch can slip in between and reopen the upstream
// without seeing the backoff. Only the scope's current watch is accounted for: an obsolete one ending
// late must not touch a newer attempt's backoff.
func (b *SharedBackend) forget(key string, s *sharedScope, end scopeEnd) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.scopes[key] != s { // a newer incarnation
		return
	}
	delete(b.scopes, key)
	if end.useful {
		delete(b.backoff, key)
	}
	switch {
	case end.failure != nil:
		b.recordFailureLocked(key, end.failure)
	case end.early:
		b.recordEarlyEndLocked(key, end.cause)
	}
}

// recordEarlyEndLocked counts a watch that ended before it was of use. The first is tolerated; a
// second in a row is a failing upstream, and backs off.
func (b *SharedBackend) recordEarlyEndLocked(key string, cause error) {
	bo := b.backoff[key]
	if bo == nil {
		// until is now, not zero, so the sweep in recordFailureLocked keeps the count for a period.
		b.backoff[key] = &sharedBackoff{until: b.now(), early: 1}
		return
	}
	if bo.early++; bo.early < 2 {
		return
	}
	unavailable := UpstreamUnavailable("the shared upstream watch keeps ending before it is of use; retry later", 0)
	unavailable.Cause = cause
	b.recordFailureLocked(key, unavailable)
}

// sharedScope is one upstream watch, its warm cache, and everyone reading from it.
type sharedScope struct {
	backend *SharedBackend
	key     string
	scope   Scope
	watcher Watcher
	cancel  context.CancelFunc

	mu       sync.Mutex
	cache    map[string]KRMObject // uid -> the object's current state
	synced   bool                 // has the upstream snapshot completed at least once?
	syncedAt time.Time            // when it first did
	dead     bool
	subs     map[*subscriber]struct{}
}

// usefulLocked reports whether this watch completed its snapshot and stayed live for minUsefulCycle.
// Called with s.mu held.
func (s *sharedScope) usefulLocked() bool {
	return s.synced && s.backend.now().Sub(s.syncedAt) >= minUsefulCycle
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
	scope *sharedScope
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
	sub.scope.backend.observe(Observation{Kind: ObservationSharedSubscriptionClosed, Scope: sub.scope.scope})
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
	sub := s.attachLocked()
	return &sharedWatcher{scope: s, sub: sub}, nil
}

// attachLocked adds one subscriber to a live scope. Called with s.mu held.
func (s *sharedScope) attachLocked() *subscriber {
	sub := &subscriber{
		scope:    s,
		ch:       make(chan WatchEvent, s.backend.queueDepth),
		ready:    make(chan struct{}, 1),
		awaiting: true,
	}
	s.subs[sub] = struct{}{}
	s.backend.observe(Observation{Kind: ObservationSharedSubscriptionOpened, Scope: s.scope})

	// The warm cache, and the entire point of the exercise: if the upstream snapshot is already
	// complete, this consumer gets its whole reset…synced now, from memory, and the API server never
	// hears about it.
	if s.synced {
		s.deliverSnapshotLocked(sub)
	}
	return sub
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
		sub.scope.backend.observe(Observation{Kind: ObservationSharedOverflow, Scope: sub.scope.scope})
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
				if !s.synced {
					s.syncedAt = s.backend.now()
				}
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
	s.mu.Lock()
	useful := s.usefulLocked()
	s.mu.Unlock()
	end := scopeEnd{useful: useful, cause: cause}
	// A StreamError decides before ErrWatchClosed does, here and below: StreamError.Unwrap exposes
	// its Cause, so a terminal FORBIDDEN caused by a closed watch also matches ErrWatchClosed.
	var se *StreamError
	typed := errors.As(cause, &se) && se != nil
	closed := !typed && (cause == nil || errors.Is(cause, ErrWatchClosed))
	if typed {
		switch {
		case !se.Terminal && se.Code == CodeUpstreamUnavailable:
			end.failure = cause
		case !se.Terminal && se.Code == CodeResyncRequired:
			end.early = !useful
		}
	} else if closed {
		end.early = !useful
	}
	s.backend.forget(s.key, s, end)
	s.cancel()

	// Whatever ended the upstream — a clean close, a 410, a cancelled context — means one thing to a
	// subscriber: continuity is lost, start a new cycle. Say it as a non-terminal RESYNC_REQUIRED so
	// the stream loop announces it and resnapshots, rather than tearing the browser's connection down.
	// A typed error is passed on as it is, with its code, terminal flag, message and hint.
	if closed {
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
	useful := s.usefulLocked()
	s.mu.Unlock()

	if empty {
		// Nobody is watching any more: not a failure, but a useful watch still resets the backoff.
		s.backend.forget(s.key, s, scopeEnd{useful: useful})
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
