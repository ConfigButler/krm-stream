package gateway

import (
	"context"
	"errors"
	"net/http"
)

// Handler is the paved road: it parses the scope from the query, resolves the principal, checks the
// scope against the allowlist and serves the stream. Gateway.ServeStream remains for hosts that route,
// name or authorize scopes their own way.

// Options configures a Handler. Everything without a default is required, and Handler panics at
// construction if one is missing or an option is unsafe, so misconfiguration fails at startup rather
// than on a request. The settings a stream needs however it is served live in the embedded
// StreamConfig; Authorizer and Clients there are required too.
type Options struct {
	StreamConfig

	// Principal resolves who is calling from the request: a session cookie, an mTLS peer, a header
	// your ingress set. Required; any default would be a policy decision about the host's auth system.
	//
	// Return a *StreamError to choose what the caller is told: Unauthenticated when signing in again
	// will help, UpstreamUnavailable when your session store is down. Any other error is sent as
	// UNAUTHENTICATED "not authenticated", and its text goes only to Diagnostics.
	Principal func(*http.Request) (Principal, error)

	// Scopes is the allowlist (spec §8). The zero value streams nothing. Setting both Resources and
	// AnyResource panics.
	Scopes ScopePolicy
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
		panic("krm-stream: StreamConfig.Authorizer is required — use gateway.AllowAll{} to say you meant it")
	case o.Clients == nil:
		panic("krm-stream: StreamConfig.Clients is required — the library holds no cluster connection of its own")
	case o.Scopes.AnyResource && len(o.Scopes.Resources) > 0:
		panic("krm-stream: ScopePolicy sets both Resources and AnyResource — choose an allowlist or delegation, not both")
	}

	g := &Gateway{StreamConfig: o.StreamConfig}

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

		g.ServeStream(w, r, principal, scope, Projection(r.URL.Query().Get("projection")))
	})
}

// refuse writes an error as a well-formed one-event stream, and closes.
func (g *Gateway) refuse(w http.ResponseWriter, r *http.Request, d Diagnostic, serr *StreamError) {
	g.diagnose(d)
	g.serveHTTP(w, r, func(ctx context.Context, sink *SSESink) {
		_ = (&sequenceSink{sink: sink}).Emit(ctx, serr.Event())
	})
}
