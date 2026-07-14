package gateway

import (
	"context"
	"fmt"
)

// The seams. This file is the whole of CONTRIBUTING's one rule, expressed as Go:
//
//	krm-stream ──depends on──▶ nothing of the host's. Ever.
//
// The gateway does not know what a tenant is, which namespace a caller "owns", where a kubeconfig
// lives, or how a session cookie becomes a person. Every time it seems to need one of those, that is
// a missing INTERFACE, not a missing import — so it asks the host, here, and the host answers.
//
// The upstream is behind an interface for the same reason, and it buys the test suite as a
// side-effect: the conformance corpus drives a scripted Backend, the k3d suite drives a real
// client-go one, and the stream loop cannot tell the difference. That is not a testing trick — it is
// what "the protocol is backend-agnostic" (spec §5, §6) has to mean if it means anything.

// Principal is whoever the host says is calling. The library never inspects it, never persists it,
// and never logs it — it carries it back to the host on ClientFor, so the host can reach the API
// server AS that caller (gateway spec §6). `any` is not laziness here: the moment this library has
// an opinion about what an identity looks like, it has an opinion about someone's auth system.
type Principal any

// Authorizer decides whether a principal may open a scope, BEFORE any watch is opened. Denial is the
// default: a gateway that opens the watch first and filters later has already leaked the object's
// existence, and possibly its contents, to someone who may not see it.
//
// Return a *StreamError to deny with a specific code (FORBIDDEN, SCOPE_INVALID); any other error is
// treated as INTERNAL.
type Authorizer interface {
	Authorize(ctx context.Context, principal Principal, scope Scope) error
}

// AllowAll authorizes everything. It exists for tests and for a single-tenant operator who has
// already decided that anyone reaching this handler may see this scope — and it is deliberately
// tedious to opt into, because the type name is what shows up in the reviewer's diff.
type AllowAll struct{}

// Authorize permits every scope.
func (AllowAll) Authorize(context.Context, Principal, Scope) error { return nil }

// AuthorizerFunc adapts a function to Authorizer.
type AuthorizerFunc func(ctx context.Context, principal Principal, scope Scope) error

// Authorize calls f.
func (f AuthorizerFunc) Authorize(ctx context.Context, p Principal, s Scope) error {
	return f(ctx, p, s)
}

// ClientFor hands the gateway a Backend for one allowlisted target, reaching the API server as the
// given principal. The host resolves BOTH halves — which cluster/workspace `target` names, and how
// this caller's identity is carried there (impersonation, an exchanged token, whatever it does).
//
// The browser never supplies an API-server URL, so this is also the choke point that makes that
// impossible rather than merely forbidden: there is nowhere to put one.
type ClientFor func(ctx context.Context, target string, principal Principal) (Backend, error)

// Backend is the upstream: one Kubernetes API server (or anything that behaves like one).
//
// Watch opens a snapshot-then-live stream for a scope. The gateway expects it to behave like a
// modern streaming list (gateway spec §3a): the objects currently in scope arrive as WatchAdded,
// terminated by a WatchBookmark whose InitialEventsEnd is true, and everything after that bookmark
// is live. A list-then-watch backend synthesizes exactly the same shape (§3b) — which is the point
// of naming the boundary rather than the mechanism.
type Backend interface {
	Watch(ctx context.Context, scope Scope) (Watcher, error)
}

// Watcher is a pull-based upstream watch.
//
// Pull, not a channel, and this is a considered choice: Next returning is the gateway's proof that
// it finished with the previous event, which makes both the conformance replay and the coalescing
// logic deterministic instead of racy. A channel-based client-go watch adapts to this in ten lines
// (see KubeBackend); the reverse — recovering a synchronisation point from a channel — is not
// possible at all.
type Watcher interface {
	// Next blocks until the next upstream event, ctx is done, or the watch ends.
	//
	// It returns ErrWatchClosed when the upstream watch ended cleanly (an API server times a watch
	// out routinely). That is NOT an error: it means "reopen", and the gateway does — with a fresh
	// snapshot cycle, because it can no longer promise it saw everything in between.
	Next(ctx context.Context) (WatchEvent, error)
	Stop()
}

// WatchEventType is the upstream vocabulary — Kubernetes's, not the protocol's. The gateway's whole
// job is translating between the two, and the translation is lossy on purpose: BOOKMARK and ERROR
// never reach a browser (spec §2).
type WatchEventType string

// The upstream event types.
const (
	WatchAdded    WatchEventType = "added"
	WatchModified WatchEventType = "modified"
	WatchDeleted  WatchEventType = "deleted"
	// WatchBookmark carries no object the consumer cares about. InitialEventsEnd marks the snapshot
	// boundary; every other bookmark is absorbed for its resourceVersion and never forwarded.
	WatchBookmark WatchEventType = "bookmark"
	// WatchError is upstream trouble. Continuity-losing errors (410 Gone, an expired
	// resourceVersion, a cache reset) mean "start a new snapshot cycle", not "give up".
	WatchError WatchEventType = "error"
)

// WatchEvent is one upstream event.
type WatchEvent struct {
	Type WatchEventType
	// Object is the complete object for added/modified, and the last-known object for deleted.
	//
	// For a deleted event it MAY be a degenerate tombstone with no usable uid — and if it is, the
	// gateway must not guess: it begins a new snapshot cycle instead of emitting an ambiguous
	// `deleted` (spec §4.2). Guessing here deletes the wrong object in someone's browser.
	Object KRMObject
	// InitialEventsEnd is set on the bookmark that closes the snapshot. It IS the `synced` boundary,
	// handed to us by the API server rather than inferred by counting objects.
	InitialEventsEnd bool
	// Err is set on WatchError.
	Err error
}

// ErrWatchClosed is what a Watcher returns when the upstream watch ended cleanly. It means "reopen",
// and the reopen begins a new snapshot cycle: a gateway that resumed silently would be promising a
// gap-free handoff it cannot deliver.
var ErrWatchClosed = fmt.Errorf("krm-stream: upstream watch closed")

// StreamError is a protocol-level error the gateway emits to the consumer, and the type a host's
// Authorizer or ClientFor returns to choose the code the browser sees.
type StreamError struct {
	Code         ErrorCode
	Message      string
	Terminal     bool
	RetryAfterMs *int
}

func (e *StreamError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Event renders the error as the wire event.
func (e *StreamError) Event() Event {
	return Event{
		Type:         EventError,
		Code:         e.Code,
		Message:      e.Message,
		Terminal:     e.Terminal,
		RetryAfterMs: e.RetryAfterMs,
	}
}

// Forbidden denies a scope. Terminal: EventSource reconnects automatically otherwise, and would
// hammer a scope it will never be allowed to see, forever.
func Forbidden(msg string) *StreamError {
	return &StreamError{Code: CodeForbidden, Message: msg, Terminal: true}
}

// ScopeInvalid rejects a scope that is not allowlisted or not resolvable.
func ScopeInvalid(msg string) *StreamError {
	return &StreamError{Code: CodeScopeInvalid, Message: msg, Terminal: true}
}

// ResyncRequired announces a loss of upstream continuity. NOT terminal: a fresh snapshot cycle
// follows on the same connection, and the consumer converges (spec §5).
func ResyncRequired(msg string) *StreamError {
	return &StreamError{Code: CodeResyncRequired, Message: msg, Terminal: false}
}

// Sink is where the gateway puts events. The SSE handler is one; the conformance suite's recorder is
// another. The stream loop knows nothing about HTTP.
type Sink interface {
	Emit(ctx context.Context, ev Event) error
}
