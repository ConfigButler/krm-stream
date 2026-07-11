// Package gateway produces a KRM resource stream (see ../spec/v1.md) from a Kubernetes watch.
//
// This file is the wire vocabulary and nothing else: the types here are what goes on the SSE
// connection, and they are the half of the contract the TypeScript client also implements. Keep it
// dependency-free — no client-go here — so that the protocol can be reasoned about, and tested,
// without a cluster anywhere in sight.
package gateway

import "encoding/json"

// ProtocolVersion is carried in the endpoint path (…/resource-stream/v1). There is no in-band
// negotiation in v1; a breaking change gets a new path segment.
const ProtocolVersion = 1

// EventType is the complete v1 vocabulary. A consumer MUST ignore types it does not know — that is
// what lets us add one later without breaking an older browser.
type EventType string

// The complete v1 event vocabulary.
const (
	// EventReset opens a snapshot cycle: mark every known uid unseen. Do NOT prune yet.
	EventReset EventType = "reset"
	// EventAdded is an upsert: "here is this object's current complete state".
	//
	// EventAdded and EventModified are the only two upsert spellings, and a consumer must treat
	// them identically. They stay distinct because they are honest KRM (they are what the watch
	// said) and because a UI legitimately animates an arrival differently from a change.
	EventAdded EventType = "added"
	// EventModified is an upsert; see EventAdded — for state purposes the two are identical.
	EventModified EventType = "modified"
	// EventDeleted carries an identity, not necessarily an object — there may not be one.
	EventDeleted EventType = "deleted"
	// EventSynced closes a snapshot cycle. Pruning happens HERE and nowhere else.
	EventSynced EventType = "synced"
	// EventError carries a machine-readable code; if terminal, it is the last event on the wire.
	EventError EventType = "error"
)

// ErrorCode is the stable, machine-readable set. A free-form message alone is not enough for a
// client to behave correctly (in particular: whether to reconnect, and whether to give up).
type ErrorCode string

// The v1 error codes. `terminal` on the event — not the code — is what says whether to give up,
// because INTERNAL can legitimately be either.
const (
	CodeForbidden           ErrorCode = "FORBIDDEN"            // terminal: lost (or never had) access
	CodeUnauthenticated     ErrorCode = "UNAUTHENTICATED"      // terminal: credential expired/rejected
	CodeScopeInvalid        ErrorCode = "SCOPE_INVALID"        // terminal: scope not allowlisted
	CodeUpstreamUnavailable ErrorCode = "UPSTREAM_UNAVAILABLE" // retryable: can't reach the API server
	CodeResyncRequired      ErrorCode = "RESYNC_REQUIRED"      // retryable: continuity lost, a new cycle follows
	CodeSlowConsumer        ErrorCode = "SLOW_CONSUMER"        // terminal: fell too far behind, dropped
	CodeInternal            ErrorCode = "INTERNAL"             // either; `terminal` says whether to give up
)

// Projection names what the stream removed from every object. It is on the wire, per cycle, because
// a consumer must be able to tell "the server does not have this field" from "the gateway took it
// away" — and no amount of squinting at a value can distinguish those.
type Projection string

const (
	// ProjectionRaw removes only machinery a human editor must never see or round-trip:
	// metadata.managedFields and the last-applied-configuration annotation.
	ProjectionRaw Projection = "krm-raw/v1"
	// ProjectionEditor removes the above, plus the gateway's declared Secret disclosure policy
	// (keys-only by default: you may see that `token` exists; you may not see or overwrite it).
	ProjectionEditor Projection = "krm-editor/v1"
)

// KRMObject is the complete, unstructured JSON of a Kubernetes object, minus exactly what the
// declared projection removed.
//
// It is deliberately NOT a struct. A ConfigMap has `data` and no `spec`; a Secret has `type` and
// `stringData`; a CRD may put any field at the root. A gateway that "helpfully" normalizes into a
// schema silently corrupts every CRD it has never heard of — so we carry the map, verbatim.
type KRMObject map[string]any

// Identity is what a `deleted` event carries. `uid` is what the consumer acts on; the rest exists
// for logs, for UI messages ("app/web was deleted"), and for defensive validation.
//
// If the gateway cannot recover a trustworthy uid (a degenerate informer tombstone), it MUST NOT
// emit an ambiguous `deleted` — it begins a new snapshot cycle instead and lets reset…synced prune.
type Identity struct {
	UID        string `json:"uid"`
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

// Scope is the normalized, server-validated target of a stream. The browser asks for a LOGICAL
// scope; the server maps it to an allowlisted target + GVR + a namespace the caller may actually
// see. A raw API-server URL is never accepted.
type Scope struct {
	Target        string `json:"target"`
	Group         string `json:"group,omitempty"`
	Version       string `json:"version"`
	Resource      string `json:"resource"`
	Namespace     string `json:"namespace,omitempty"` // absent => a cluster-scoped resource
	Name          string `json:"name,omitempty"`      // present => a single-object scope
	LabelSelector string `json:"labelSelector,omitempty"`
}

// Event is one framed message on the stream. Heartbeats are SSE comments (": heartbeat"), not
// events, and are a no-op to consumers.
type Event struct {
	Type EventType `json:"type"`

	// reset
	Target     string     `json:"target,omitempty"`
	Scope      *Scope     `json:"scope,omitempty"`
	Projection Projection `json:"projection,omitempty"`

	// added / modified
	Object KRMObject `json:"object,omitempty"`
	// RedactedPaths is a REQUIRED array on every added/modified (empty, not absent, when nothing is
	// redacted) — RFC 6901 JSON Pointers into Object. It is mandatory precisely so that a consumer
	// never has to infer redaction from a value that merely looks like a placeholder.
	RedactedPaths []string `json:"redactedPaths,omitempty"`

	// deleted
	Identity *Identity `json:"identity,omitempty"`

	// error
	Code         ErrorCode `json:"code,omitempty"`
	Message      string    `json:"message,omitempty"`
	Terminal     bool      `json:"terminal,omitempty"`
	RetryAfterMs *int      `json:"retryAfterMs,omitempty"`
}

// MarshalJSON exists for one field: `redactedPaths` is REQUIRED on every added/modified — present,
// not merely optional — and `omitempty` would silently drop the empty array, which is precisely the
// case the requirement is about ("nothing is redacted in this object" must be SAID, not inferred).
// It stays omitted on the events that have no business carrying it.
//
// The conformance suite caught this the first time it ran. That is the corpus doing its job.
func (e Event) MarshalJSON() ([]byte, error) {
	type base Event // sheds this method, so json doesn't recurse
	if e.Type != EventAdded && e.Type != EventModified {
		return json.Marshal(base(e))
	}
	paths := e.RedactedPaths
	if paths == nil {
		paths = []string{}
	}
	inner := base(e)
	inner.RedactedPaths = nil // the outer, non-omitempty field carries it (shallower field wins)
	return json.Marshal(struct {
		base
		RedactedPaths []string `json:"redactedPaths"`
	}{inner, paths})
}

// UID reads metadata.uid out of an object. The empty string means "this object has no usable
// identity", which is a bug upstream of here and must never be forwarded as added/modified.
func (o KRMObject) UID() string {
	meta, ok := o["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	uid, _ := meta["uid"].(string)
	return uid
}

// MarshalSSE frames one event as an SSE `data:` line, ready to write to the connection.
//
// Note what is NOT here: an SSE `id:` line. v1 forbids it. Every new connection begins a complete
// snapshot cycle, so there is no delta replay and no Last-Event-ID resume to support — and putting
// a resource uid in `id:` (the tempting thing) would give the browser's automatic reconnect an
// entirely incorrect meaning.
func (e Event) MarshalSSE() ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(b)+8)
	out = append(out, "data: "...)
	out = append(out, b...)
	out = append(out, '\n', '\n')
	return out, nil
}
