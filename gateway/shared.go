package gateway

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// SharedBackend fans one upstream watch per scope out to every subscriber, so ten tabs on one
// namespace cost one watch and one warm cache, and a joining subscriber is snapshotted from memory.
//
// It is opt-in because the shared watch is opened once, as one identity. Without sharing, each
// stream acts as its caller and Kubernetes RBAC is the boundary. With it, every subscriber reads the
// service identity's cache, and the host's Authorizer is the only check between a caller and the
// objects. Pair it with kube.SubjectAccessReviewAuthorizer so Kubernetes still decides; see
// docs/auth.md.

// sharedQueueDepth is how many live events one subscriber may fall behind by before it is
// resnapshotted from the warm cache. The bound keeps one slow consumer from growing memory without
// limit in a process serving everyone else.
const sharedQueueDepth = 256

// The backoff after a shared upstream watch fails to open with UPSTREAM_UNAVAILABLE, dies of it, or
// keeps ending before it is of use. A scope then sees at most one attempt per backoff period, however
// many subscribers reconnect; the rest are told when to come back.
//
// A watch is of use once it completed its snapshot and stayed live for minUsefulCycle; only such a
// watch's end resets the backoff. One early end is tolerated (§5); a second in a row backs off.
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

// SharedBackend multiplexes many consumers onto one upstream watch per scope. It is a Backend, so it
// drops in behind the same seam as any other.
//
// b.mu guards scopes, openings and backoff, and is never held across an upstream call. Lock order is
// b.mu before a sharedScope's mu.
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

// sharedOpening is one scope's upstream watch while it is being opened: registered under b.mu, opened
// without it, and settled under it again. Opening is a network round trip of unknown length, so it
// must hold up only its own scope's callers, each of whom can leave; the last to leave cancels it.
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
	// subs holds one subscription per waiter when the opening succeeds, attached when the scope is
	// published, so the scope never holds an upstream watch without someone who will release it.
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

// NewSharedBackend shares one upstream watch per scope across every consumer of it. The upstream is
// opened as whatever identity `upstream` carries, so your Authorizer becomes the security boundary.
// The zero SharedOptions{} uses the default queue depth and no observer.
func NewSharedBackend(upstream Backend, options SharedOptions) *SharedBackend {
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
// ctx bounds only this caller's wait, never the upstream watch, which belongs to every subscriber.
// The upstream has a context of its own, cancelled while opening by the last waiter to leave, and
// once open by the last subscriber to Stop (see leave).
func (b *SharedBackend) Watch(ctx context.Context, scope Scope) (Watcher, error) {
	// A caller that has already gone neither starts nor joins an opening.
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

// settle publishes an opening's result to its waiters. An opening every waiter abandoned is
// discarded: its watcher is stopped, and it touches neither the scope map, the backoff, nor any
// opening that replaced it.
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
		// Attach every waiter before the scope is visible, so its last departure cancels it.
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

// abandon withdraws a waiter whose context ended. Before the opening settles, the waiter stops being
// counted, and the last one cancels and unregisters the opening. If it settled first, the waiter
// owns a subscription and releases it as a subscriber would.
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

// recordFailureLocked starts or doubles a scope's backoff after a retryable UPSTREAM_UNAVAILABLE. Any
// other failure leaves the backoff unchanged.
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

// newSharedScope wraps an opened upstream watch. cancel is the opening's, detached from every
// subscriber's request context.
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

// forget drops a dead or empty scope and accounts for how it ended in the same critical section, so
// no Watch can reopen the upstream without seeing the backoff. An obsolete scope ending late changes
// nothing.
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

// subscriber is one consumer's view of the shared watch: a snapshot, then a bounded queue of live
// events. They are kept apart because they differ: the snapshot is the consumer's starting state,
// finite and never to be dropped, so it is an unbounded slice drained first; live events are
// open-ended, and a consumer that cannot keep up with them is resnapshotted. (Sending the snapshot
// through the bounded queue made any scope larger than the queue unservable.)
type subscriber struct {
	scope *sharedScope
	// pending is the snapshot: added* and the boundary bookmark. Guarded by sharedScope.mu, and
	// drained by Next() before a single live event is read.
	pending []WatchEvent
	// ready wakes a reader already blocked on `ch` when the snapshot lands in `pending`, which never
	// travels through `ch`. Buffered 1: a reader re-checks `pending` before parking again, so no
	// signal is missed.
	ready chan struct{}
	ch    chan WatchEvent
	// awaiting is true until this subscriber has been handed its snapshot. Live events are not
	// delivered before then: the cache it will receive already contains them.
	awaiting bool
	closed   bool
	// reason is why this subscriber's queue ended. It lives beside the queue, not in it, because the
	// queue may be full when it ends. Written before close(ch), so a receiver that observes the close
	// sees it.
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

	// A warm cache snapshots the joiner from memory, without touching the API server.
	if s.synced {
		s.deliverSnapshotLocked(sub)
	}
	return sub
}

// deliverSnapshotLocked hands one subscriber the whole cache as a snapshot, terminated by the
// boundary bookmark. Called with s.mu held, so no live event can slip between the two. It cannot
// overflow: its size is the scope's, which the operator allowlisted.
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

	// Wake a reader parked on `ch`. Non-blocking: the buffer holds the one signal that matters.
	select {
	case sub.ready <- struct{}{}:
	default:
	}
}

// offer enqueues without blocking. A full queue ends the subscriber with a continuity-loss error,
// which the stream loop recovers from with a fresh snapshot off the warm cache. Blocking would let
// one stalled browser stall the pump, and every other subscriber with it.
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

// pump is the single reader of the upstream watch and the only writer of the cache: one goroutine per
// scope, for the life of the upstream watch.
func (s *sharedScope) pump(ctx context.Context) {
	defer s.watcher.Stop()

	for {
		ev, err := s.watcher.Next(ctx)
		if err != nil {
			// However the upstream ended, continuity is lost. The scope dies, and subscribers
			// resyncing at once share the one fresh upstream watch the next Watch opens.
			s.die(err)
			return
		}

		s.mu.Lock()
		switch ev.Type {
		case WatchBookmark:
			if ev.InitialEventsEnd {
				// The cache is now a true picture of the scope; hand it to everyone waiting.
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
			// Routine bookmarks are absorbed; each subscriber's boundary is synthesized above.

		case WatchAdded, WatchModified:
			// The stream loop's partial-object guard (spec §2), applied here too because the cache is
			// replayed to every future joiner.
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
				// A degenerate tombstone: guessing which object left could evict the wrong one.
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
			// Its pending snapshot already contains this event.
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

	// An untyped end is lost continuity: report it as a non-terminal RESYNC_REQUIRED so the stream
	// loop resnapshots on the same connection. A typed error is passed on unchanged.
	if closed {
		cause = ResyncRequired("the shared upstream watch ended; a new snapshot cycle follows")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.dead = true
	for sub := range s.subs {
		// The event is best-effort (a full queue is already resyncing); the close carries the reason.
		sub.offer(WatchEvent{Type: WatchError, Err: cause})
		sub.end(cause)
	}
	s.subs = map[*subscriber]struct{}{}
}

// leave removes one subscriber, and tears the scope and its upstream watch down when the last one
// goes.
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

// nextPending pops one snapshot event, if any remain, under the scope's lock that guards `pending`.
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
		// The whole snapshot first, before any live event.
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
				// Drained and closed. `reason` was written before the close, so it is visible here.
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

// scopeKey is the identity of a shared watch. Every field participates, including the label
// selector, which changes which objects are in the snapshot. `\x1f` (unit separator) cannot occur in
// any field, so no two scopes collide.
func scopeKey(s Scope) string {
	return strings.Join([]string{
		s.Target, s.Group, s.Version, s.Resource, s.Namespace, s.Name, s.LabelSelector,
	}, "\x1f")
}
