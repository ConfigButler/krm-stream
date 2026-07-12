package gateway

import (
	"context"
	"errors"
	"strings"
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

// ResourceVersionOrdering says how much this gateway may trust the upstream's resourceVersions.
//
// The protocol promises a consumer **per-object monotonicity** (spec §6): within a snapshot cycle, it
// will never be handed a state older than one it already has. That promise is what makes coalescing
// safe, and it is the one guarantee a naive "just relay the watch" gateway cannot give. The gateway
// keeps it by ORDERING resourceVersions — so what it may assume about them is a real decision, and it
// is this one.
type ResourceVersionOrdering int

const (
	// OrderingStrict requires every resourceVersion to be an orderable decimal, and is the default.
	//
	// This is not optimism. Since **Kubernetes 1.35 it is a conformance requirement**: "orderability of
	// resource versions for all Kubernetes types is included in Certified Kubernetes requirements. Base
	// API objects **and custom resources** must be orderable as a monotonically increasing integer for
	// any 1.35+ APIServer implementation in order to pass conformance tests."
	//
	// So on a supported cluster, an unorderable resourceVersion cannot happen — and if one arrives, the
	// honest response is to SAY SO and stop, not to silently drop the guarantee and keep streaming. A
	// consumer that was promised monotonicity and is quietly no longer getting it is worse off than one
	// that was told. The error names the fix (OrderingLenient), so the operator is never stuck.
	OrderingStrict ResourceVersionOrdering = iota

	// OrderingLenient tolerates a resourceVersion it cannot order, and simply does not order it.
	//
	// For the two cases the docs carve out, both of which are real:
	//
	//   - a cluster **older than 1.35**, where orderability was conventional rather than required;
	//   - an **aggregated / extension API server**, which is a third-party implementation and is not
	//     covered by that conformance test. The docs are explicit: if a resourceVersion "does not parse
	//     as a decimal number, the two strings can be checked for equality but you CANNOT rely on
	//     comparisons for ordering."
	//
	// The cost is precise and worth stating: for objects whose resourceVersions cannot be ordered, the
	// gateway can no longer drop a stale replay, so a consumer may briefly see an older state before
	// converging. It never sees a WRONG final state — a duplicate is harmless, because applying an
	// event is idempotent by construction (§6) — it just loses the "never goes backwards" property in
	// between. That is strictly better than dropping an update that was actually newer.
	OrderingLenient
)

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
	// Ordering is how far the upstream's resourceVersions may be trusted. The zero value is
	// OrderingStrict: this library targets Kubernetes 1.35+, where orderability is a conformance
	// requirement, and it says so rather than degrading quietly on every cluster to accommodate one.
	Ordering ResourceVersionOrdering
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
	emitted := map[string]string{}

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
			// A partial object must never be forwarded (spec §2). The consumer's model is REPLACE,
			// never merge, so a fragment does not "update" its object — it BLANKS it: the status view
			// goes empty and an editor loses the user's spec.
			//
			// Note what is checked, and what is NOT. "Has no uid" is not sufficient, and believing it
			// was is the bug this corrected: a PartialObjectMetadata carries a complete metadata block,
			// uid included, and only omits `spec`/`status`. The kind is the honest test. (The uid test
			// still earns its keep — a BOOKMARK's object has only a resourceVersion.)
			if reason := partialReason(ev.Object); reason != "" {
				// Resnapshot: it is the one recovery that is always correct, because it re-establishes
				// the truth instead of guessing at it.
				return ResyncRequired(reason)
			}
			obj, redacted := project(projection, ev.Object)
			uid := obj.UID()
			stale, err := isStale(g.Ordering, emitted, uid, obj)
			if err != nil {
				return err
			}
			if stale {
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

// partialReason says why an object must not be forwarded as added/modified, or "" if it may be.
//
// Both cases are things Kubernetes really sends — see docs/facts/kubernetes-api-concepts.md:
//
//  1. PartialObjectMetadata. A client can ask for metadata-only responses
//     (`Accept: application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1`), and then "the returned
//     objects only contain the `metadata` field. The `spec` and `status` fields are omitted." It KEEPS
//     ITS UID. If this gateway is ever wired to a client that negotiated such a response — or to an
//     aggregated API server that returns one — every object on the stream would arrive as a husk.
//
//  2. No uid at all. A BOOKMARK's object is exactly this: "of the type requested by the request, but
//     only includes a .metadata.resourceVersion field". Bookmarks are absorbed before we get here, so
//     reaching this branch means something upstream is confused — and confusion is not a thing to
//     forward to a browser.
func partialReason(obj KRMObject) string {
	kind, _ := obj["kind"].(string)
	if kind == "PartialObjectMetadata" || kind == "PartialObjectMetadataList" {
		return "upstream delivered a metadata-only object (" + kind + "); it has no spec and no status"
	}
	if obj.UID() == "" {
		return "upstream delivered a partial object with no uid"
	}
	return ""
}

// isStale enforces per-object monotonicity (§6): within one cycle, never hand a consumer a state
// older than one it already has. This is what makes coalescing safe, and it is the guarantee a naive
// relay cannot give — an informer that relists mid-stream will happily replay an older version of an
// object it has already delivered.
//
// It returns an error only in OrderingStrict (the default) and only when the upstream served a
// resourceVersion that cannot be ordered — which a conformant Kubernetes ≥1.35 never does. See
// ResourceVersionOrdering for why that is a refusal and not a shrug.
func isStale(ordering ResourceVersionOrdering, emitted map[string]string, uid string, obj KRMObject) (bool, error) {
	rv := resourceVersionOf(obj)
	last, seen := emitted[uid]
	if !seen {
		if ordering == OrderingStrict && !isDecimalResourceVersion(rv) {
			return false, unorderable(rv)
		}
		emitted[uid] = rv
		return false, nil
	}

	cmp, ok := compareResourceVersion(rv, last)
	if !ok {
		if ordering == OrderingStrict {
			return false, unorderable(rv)
		}
		// Lenient: we cannot order these, so we do not pretend to. Let it through — a duplicate is
		// harmless (apply is idempotent by construction), a wrongly-dropped update is data loss.
		emitted[uid] = rv
		return false, nil
	}
	if cmp < 0 {
		return true, nil // older than what the consumer already holds: drop it (§6)
	}
	emitted[uid] = rv
	return false, nil
}

func unorderable(rv string) error {
	return &StreamError{
		Code:     CodeInternal,
		Terminal: true,
		Message: "upstream served a resourceVersion that cannot be ordered (" + rv + "). " +
			"Kubernetes 1.35+ requires every resourceVersion to be an orderable decimal, so this " +
			"upstream is either older than 1.35 or an aggregated API server that does not conform. " +
			"Set Gateway.Ordering = OrderingLenient to stream it anyway, without per-object monotonicity.",
	}
}

// compareResourceVersion orders two resource versions the way Kubernetes says to. `ok` is false when
// they cannot be ordered at all — which, on a conformant 1.35+ server, never happens.
//
// This function is small and it is the second bug this project shipped for want of reading the docs.
// The old version was `strconv.ParseInt(rv, 10, 64)`. Kubernetes:
//
//	"Resource versions are compared as ARBITRARY BITSIZE decimal integers... The bitsize must not be
//	 assumed to be some fixed amount."
//
// and its own worked example is FORTY DIGITS long. int64 holds nineteen. Against such a cluster the
// parse failed, the staleness check gave up, and updates were dropped — a symptom indistinguishable
// from "Kubernetes is being slow", which is how a bug like this survives for years.
//
// The prescribed comparison, verbatim: "If they are not of equal length, the longer one is greater
// (for example, "123" > "23"). If they are of equal length, the lexicographically greater one is
// greater." Note that this rules out a PLAIN lexicographic compare, which calls "9" > "10".
func compareResourceVersion(a, b string) (cmp int, ok bool) {
	if a == b {
		return 0, isDecimalResourceVersion(a)
	}
	if !isDecimalResourceVersion(a) || !isDecimalResourceVersion(b) {
		return 0, false
	}
	if len(a) != len(b) {
		if len(a) > len(b) {
			return 1, true
		}
		return -1, true
	}
	return strings.Compare(a, b), true
}

// isDecimalResourceVersion reports whether a resource version may be ORDERED, per the rules the API
// docs give: "Both must start with a digit 1-9 and contain only digits 0-9."
func isDecimalResourceVersion(rv string) bool {
	if rv == "" || rv[0] < '1' || rv[0] > '9' {
		return false
	}
	for i := 0; i < len(rv); i++ {
		if rv[i] < '0' || rv[i] > '9' {
			return false
		}
	}
	return true
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
