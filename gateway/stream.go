package gateway

import (
	"context"
	"errors"
	"strconv"
)

// The stream loop: a Kubernetes watch in, the protocol out.
//
// Everything hard about this is in the translation, and the translation is deliberately lossy.
// Kubernetes speaks BOOKMARK, 410 Gone, relists and degenerate tombstones; a browser must never hear
// any of those words. What it hears instead is a small, stable vocabulary with exactly one framing —
// reset … added* … synced, then live deltas — for named and collection scopes alike.
//
// The four rules that a "just relay the watch" gateway gets wrong, and that everything below exists
// to enforce:
//
//  1. A snapshot cycle is not tied to a connection. Lose upstream continuity and you begin a NEW
//     cycle on the same healthy SSE connection (§5) — otherwise the ghost you leave is invisible
//     until someone notices a deleted object still on screen.
//  2. A partial object is never forwarded. It would BLANK the consumer's state for that uid.
//  3. An ambiguous tombstone is never emitted. A guessed uid deletes the wrong object.
//  4. Never emit, within a cycle, a state older than one already emitted for that uid (§6).

// Gateway turns a Kubernetes watch into a conforming resource stream. It holds no cluster
// connection of its own: the host supplies both the authorization decision and the client, through
// the seams in seams.go.
type Gateway struct {
	// Auth decides whether a principal may open a scope, before any watch opens. Required.
	Auth Authorizer
	// Clients resolves (target, principal) to an upstream. Required.
	Clients ClientFor
	// Projection is what this stream removes and masks. Defaults to krm-editor/v1 — the safe one:
	// a gateway that defaults to raw and streams Secret values because someone forgot a config line
	// has a vulnerability, not a bug.
	Projection Projection
}

// Stream runs one consumer's stream until the context is done or a terminal error is emitted.
//
// It returns after emitting a terminal error; the caller (the SSE handler) must then CLOSE the
// connection. A browser's EventSource reconnects automatically otherwise, and would hammer a
// forbidden scope forever.
func (g *Gateway) Stream(ctx context.Context, principal Principal, scope Scope, sink Sink) error {
	projection := g.Projection
	if projection == "" {
		projection = ProjectionEditor
	}

	// Denial comes FIRST, before any watch is opened. A gateway that opens the watch and filters
	// afterwards has already leaked the object's existence — and, if it logs, its contents.
	if err := g.Auth.Authorize(ctx, principal, scope); err != nil {
		return emitTerminal(ctx, sink, err)
	}

	backend, err := g.Clients(scope.Target, principal)
	if err != nil {
		return emitTerminal(ctx, sink, err)
	}

	for cycles := 0; ; cycles++ {
		if cycles > 0 {
			// A new cycle on a live connection. Say so first: the consumer is about to be told a
			// whole new snapshot, and RESYNC_REQUIRED is what distinguishes "we lost continuity and
			// are recovering" from "your objects all changed at once". Non-terminal — recovery is
			// the normal case, not the failure case.
			if err := sink.Emit(ctx, ResyncRequired("upstream continuity lost; a new snapshot cycle follows").Event()); err != nil {
				return err
			}
		}

		err := g.cycle(ctx, backend, scope, projection, sink)
		switch {
		case err == nil:
			// A cycle only ends by error or cancellation; nil would be a bug in the loop below.
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrWatchClosed):
			// A routine watch timeout. Reopen — with a full snapshot, because between the close and
			// the reopen we can promise nothing, and the protocol's whole value is that what you
			// end up holding is right.
			continue
		}

		var se *StreamError
		if errors.As(err, &se) && !se.Terminal {
			continue // a recoverable upstream error: announce, resnapshot, carry on
		}
		return emitTerminal(ctx, sink, err)
	}
}

// cycle runs exactly one snapshot cycle and the live tail that follows it, returning when upstream
// continuity is lost (or the context is done).
func (g *Gateway) cycle(ctx context.Context, backend Backend, scope Scope, projection Projection, sink Sink) error {
	w, err := backend.Watch(ctx, scope)
	if err != nil {
		return err
	}
	defer w.Stop()

	if err := sink.Emit(ctx, Event{Type: EventReset, Target: scope.Target, Scope: &scope, Projection: projection}); err != nil {
		return err
	}

	// Per-uid high-water mark, and it is per CYCLE, not per stream: the protocol's monotonicity
	// promise is scoped to a cycle (§6), and a new cycle legitimately re-delivers a state the
	// consumer already saw (that is what makes reconnect replays idempotent). Keeping this map
	// across cycles would silently swallow the snapshot of an object whose resourceVersion had not
	// moved — the object would simply never arrive, and the consumer would prune it.
	emitted := map[string]int64{}

	for {
		ev, err := w.Next(ctx)
		if err != nil {
			return err
		}

		switch ev.Type {
		case WatchBookmark:
			// THE snapshot boundary, handed to us by the API server rather than inferred by counting
			// objects. Every other bookmark is absorbed for its resourceVersion and never forwarded:
			// a consumer that saw one would have to know what it was (spec §2).
			if ev.InitialEventsEnd {
				if err := sink.Emit(ctx, Event{Type: EventSynced}); err != nil {
					return err
				}
			}

		case WatchAdded, WatchModified:
			obj, redacted := project(projection, ev.Object)
			uid := obj.UID()
			if uid == "" {
				// A metadata-only or otherwise partial object. Forwarding it as added/modified would
				// REPLACE the consumer's object with a fragment — the consumer's whole model is
				// "replace, never merge" — so the object would blank out on screen. Resnapshot
				// instead: it is the one recovery that is always correct.
				return ResyncRequired("upstream delivered a partial object with no uid")
			}
			if isStale(emitted, uid, obj) {
				continue
			}
			out := Event{Type: EventAdded, Object: obj, RedactedPaths: redacted}
			if ev.Type == WatchModified {
				out.Type = EventModified
			}
			if err := sink.Emit(ctx, out); err != nil {
				return err
			}

		case WatchDeleted:
			id := identityOf(ev.Object)
			if id == nil {
				// A degenerate tombstone. We will NOT guess a uid — that deletes the wrong object
				// out of someone's browser. A new snapshot cycle removes it correctly instead, at
				// the cost of one relist (spec §4.2).
				return ResyncRequired("deletion tombstone carried no trustworthy uid")
			}
			// No `object` on the tombstone: the protocol makes it optional ("when the gateway has
			// it"), and the final state of an object nobody can see any more is not worth the bytes
			// or the second code path. The identity is what a consumer acts on.
			if err := sink.Emit(ctx, Event{Type: EventDeleted, Identity: id}); err != nil {
				return err
			}
			delete(emitted, id.UID)

		case WatchError:
			if ev.Err != nil {
				return ev.Err
			}
			return ResyncRequired("upstream error")
		}
	}
}

// isStale enforces per-object monotonicity (§6): within one cycle, never hand a consumer a state
// older than one it already has. This is what makes coalescing safe, and it is the guarantee a naive
// relay cannot give — an informer that relists mid-stream will happily replay an older version of an
// object it has already delivered.
//
// A resourceVersion that does not parse as an integer is not compared. The gateway may rely on
// monotonicity within one target (§3d) but must not CRASH on a backend that does not provide it: the
// failure mode of a wrong guess here is a dropped update, which is worse than a duplicate one.
func isStale(emitted map[string]int64, uid string, obj KRMObject) bool {
	rv, err := strconv.ParseInt(resourceVersionOf(obj), 10, 64)
	if err != nil {
		return false
	}
	if last, ok := emitted[uid]; ok && rv < last {
		return true
	}
	emitted[uid] = rv
	return false
}

// emitTerminal puts the error on the wire and returns it. A terminal error MUST be the last event on
// the connection, and the caller closes it.
func emitTerminal(ctx context.Context, sink Sink, err error) error {
	var se *StreamError
	if !errors.As(err, &se) {
		se = &StreamError{Code: CodeInternal, Message: err.Error(), Terminal: true}
	}
	if emitErr := sink.Emit(ctx, se.Event()); emitErr != nil {
		return emitErr
	}
	return se
}
