package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// Handler is the paved road: it parses the scope from the query, resolves the principal, checks the
// scope against the allowlist and serves the stream. ServeStream remains for hosts that route, name
// or authorize scopes their own way.

// Options configures a Handler. Everything without a default is required, and Handler panics at
// construction if one is missing or an option is unsafe, so misconfiguration fails at startup rather
// than on a request.
type Options struct {
	// Principal resolves who is calling from the request: a session cookie, an mTLS peer, a header
	// your ingress set. Required; any default would be a policy decision about the host's auth system.
	//
	// Return a *StreamError to choose what the caller is told: Unauthenticated when signing in again
	// will help, UpstreamUnavailable when your session store is down. Any other error is sent as
	// UNAUTHENTICATED "not authenticated", and its text goes only to Diagnostics.
	Principal func(*http.Request) (Principal, error)

	// Authorizer decides whether that principal may open this scope, before any watch is opened.
	// Required; gateway.AllowAll{} permits everything explicitly.
	Authorizer Authorizer

	// Clients resolves (target, principal) to an upstream. Required.
	Clients ClientFor

	// Scopes is the allowlist (spec §8). The zero value streams nothing. Setting both Resources and
	// AnyResource panics.
	Scopes ScopePolicy

	// Projection defaults to ProjectionFull, which redacts Secret values.
	Projection Projection

	// Projections authorizes a requested projection for a principal and scope. It is optional only
	// because Projection supplies a safe static policy for hosts that expose one view.
	Projections ProjectionPolicy

	// Ordering defaults to OrderingStrict (Kubernetes 1.35+ conformance). See stream.go.
	Ordering ResourceVersionOrdering

	// Observer receives low-cardinality stream lifecycle signals. It must not block.
	Observer Observer

	// Diagnostics receives the raw error behind every error event, including the detail the wire
	// does not carry. It must not block. Nil discards it.
	Diagnostics Diagnostics

	// HeartbeatInterval defaults to HeartbeatInterval. Set a positive value to match a proxy's idle
	// timeout; it affects HTTP streams only.
	HeartbeatInterval time.Duration

	// WriteTimeout bounds each HTTP write-plus-flush operation. Zero installs no deadline.
	// Positive values require a writer supporting flush and write deadlines; negative values panic.
	// Required (positive) whenever ReauthorizationInterval is.
	WriteTimeout time.Duration

	// ReauthorizationInterval rechecks each subscriber independently, even on quiet streams.
	// Zero disables timed checks; snapshot cycles always reauthorize.
	//
	// A positive interval requires a positive WriteTimeout, and Handler panics without one. A timed
	// check waits for that subscriber's write in progress, and a write to a browser that stopped
	// reading only ends at its deadline. The revocation budget is documented in docs/auth.md.
	ReauthorizationInterval time.Duration
	// ReauthorizationTimeout bounds each timed check's Authorizer and projection-policy callbacks,
	// starting once the check holds the subscriber's delivery gate. Zero defaults to 10 seconds.
	// Authorizers and projection policies must honor context cancellation.
	ReauthorizationTimeout time.Duration
}

// Handler mounts the stream on one route.
//
// Every rejection (a malformed or disallowed scope, an unidentifiable caller) is delivered as a
// terminal SSE error event over a 200, not as an HTTP status: EventSource cannot read a non-200 body,
// and `terminal` stops it reconnecting. Unsupported bounded transports instead abort before streaming
// and report ObservationHTTPTransportRejected.
func Handler(o Options) http.Handler {
	validateHTTPServing(o.WriteTimeout, o.ReauthorizationInterval)
	switch {
	case o.Principal == nil:
		panic("krm-stream: Options.Principal is required — the library must never assume who the caller is")
	case o.Authorizer == nil:
		panic("krm-stream: Options.Authorizer is required — use gateway.AllowAll{} to say you meant it")
	case o.Clients == nil:
		panic("krm-stream: Options.Clients is required — the library holds no cluster connection of its own")
	case o.Scopes.AnyResource && len(o.Scopes.Resources) > 0:
		panic("krm-stream: ScopePolicy sets both Resources and AnyResource — choose an allowlist or delegation, not both")
	}

	g := &Gateway{
		Auth:                    o.Authorizer,
		Clients:                 o.Clients,
		Projection:              o.Projection,
		Projections:             o.Projections,
		Ordering:                o.Ordering,
		Observer:                o.Observer,
		Diagnostics:             o.Diagnostics,
		HeartbeatInterval:       o.HeartbeatInterval,
		WriteTimeout:            o.WriteTimeout,
		ReauthorizationInterval: o.ReauthorizationInterval,
		ReauthorizationTimeout:  o.ReauthorizationTimeout,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := o.Principal(r)
		if err != nil {
			var se *StreamError
			if !errors.As(err, &se) || se == nil {
				se = &StreamError{Code: CodeUnauthenticated, Message: "not authenticated", Terminal: true, Cause: err}
			}
			g.refuse(w, r, Diagnostic{Code: se.Code, Terminal: se.Terminal, Err: err}, se)
			return
		}

		scope, err := ScopeFromQuery(r.URL.Query())
		if err == nil {
			err = o.Scopes.Validate(scope)
		}
		if err != nil {
			se := asStreamError(err)
			g.refuse(w, r, Diagnostic{Principal: principal, Scope: scope, Code: se.Code, Terminal: se.Terminal, Err: err}, se)
			return
		}

		g.ServeStreamProjection(w, r, principal, scope, Projection(r.URL.Query().Get("projection")))
	})
}

// refuse writes an error as a well-formed one-event stream, and closes.
func (g *Gateway) refuse(w http.ResponseWriter, r *http.Request, d Diagnostic, serr *StreamError) {
	g.diagnose(d)
	g.serveHTTP(w, r, func(ctx context.Context, sink *SSESink) {
		_ = (&sequenceSink{sink: sink}).Emit(ctx, serr.Event())
	})
}
