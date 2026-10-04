package gateway

import "time"

// StreamConfig is everything a stream needs however it is served. Options embeds it for Handler and
// Gateway embeds it for ServeStream and Stream, so each setting has one name on both paths and Handler
// copies the whole value rather than mapping it field by field.
//
// Go composite literals name the embedded value:
//
//	gateway.Options{
//		Principal: userFromSession,
//		Scopes:    allowedScopes,
//		StreamConfig: gateway.StreamConfig{
//			Authorizer:   authorizeScope,
//			Clients:      clientsForUser,
//			Projections:  gateway.StaticProjection(gateway.ProjectionSpec),
//			WriteTimeout: 5 * time.Second,
//		},
//	}
type StreamConfig struct {
	// Authorizer decides whether a principal may open a scope, before any watch opens, and again on
	// every snapshot cycle and timed check. Required; gateway.AllowAll{} permits everything explicitly.
	Authorizer Authorizer

	// Clients resolves (target, principal) to an upstream. Required.
	Clients ClientFor

	// Projections selects the projection authorized for a principal, scope and requested name. Nil
	// grants only ProjectionFull, the safe default: it redacts Secret values, because a gateway that
	// streams them raw because someone forgot a line of configuration has a vulnerability, not a bug.
	// Use StaticProjection for a host with one view, or a ProjectionPolicy that selects by principal
	// and scope. A browser may request a name but never supplies projection rules.
	Projections ProjectionPolicy

	// Ordering is how far the upstream's resourceVersions may be trusted. The zero value is
	// OrderingStrict: this library targets Kubernetes 1.35+, where orderability is a conformance
	// requirement. Use OrderingLenient only for a known aggregated API that cannot order them.
	Ordering ResourceVersionOrdering

	// Observer receives low-cardinality stream lifecycle signals. It must not block. Nil disables
	// observations.
	Observer Observer

	// Diagnostics receives the raw error behind every error event, including the detail the wire does
	// not carry. It must not block. Nil discards it.
	Diagnostics Diagnostics

	// HeartbeatInterval is how often an idle HTTP stream sends an SSE heartbeat comment. Zero uses
	// HeartbeatInterval (20 seconds); set a positive value to match a proxy's idle timeout. It has no
	// effect on the transport-neutral Stream method.
	HeartbeatInterval time.Duration

	// WriteTimeout bounds each HTTP write-plus-flush operation. Zero installs no deadline. Positive
	// values require a writer supporting flush and write deadlines; negative values panic before any
	// response I/O. It does not bound generic Stream sinks, callbacks or time waiting for delivery locks.
	WriteTimeout time.Duration

	// ReauthorizationInterval rechecks each subscriber independently, even on quiet streams. Zero
	// disables timed checks; snapshot cycles always reauthorize.
	//
	// A timed check waits for the subscriber's delivery in progress, so it can only be as prompt as the
	// sink lets it be. HTTP serving (Handler and ServeStream) therefore requires a positive WriteTimeout
	// with it, and panics before writing anything otherwise: a write to a browser that stopped reading
	// only ends at its deadline. Stream does not check this; its sink is the host's, and the host must
	// bound its I/O and honor cancellation. The revocation budget is documented in docs/auth.md.
	ReauthorizationInterval time.Duration

	// ReauthorizationTimeout bounds each timed check's Authorizer and projection-policy callbacks,
	// starting once the check holds the subscriber's delivery gate. Zero defaults to 10 seconds.
	// Authorizers and projection policies must honor context cancellation.
	ReauthorizationTimeout time.Duration
}
