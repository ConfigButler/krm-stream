package gateway

import (
	"context"
	"net/http"
)

// The http.Handler the README has been promising, and which did not exist.
//
// That is worth saying plainly rather than quietly fixing: the first code block in the project's
// README called `gateway.Handler(gateway.Options{...})`, and neither identifier was real. The first
// thing a new adopter copies did not compile. What existed instead was ServeStream(w, r, principal,
// scope) — which is a fine seam, but it left every host to write the same four things by hand: parse
// the scope out of the query, decide what a principal is, check the scope against an allowlist, and
// wire the three together. Four chances to get security-relevant glue subtly wrong, per adopter.
//
// So the library now ships the glue, and it is one line to mount:
//
//	mux.Handle("/resource-stream/v1", gateway.Handler(gateway.Options{
//	    Principal:  func(r *http.Request) (gateway.Principal, error) { return userFrom(r) },
//	    Authorizer: myAuthz,
//	    Clients:    myClientFor,
//	    Scopes:     gateway.ScopePolicy{Targets: []string{""}, Resources: []gateway.GroupResource{{Resource: "configmaps"}}},
//	}))
//
// ServeStream stays exported. A host that wants to route, name or authorize scopes its own way still
// can — the Handler is the paved road, not a wall.

// Options configures a Handler. Everything without a default is required, and Handler PANICS if one
// is missing — at mount time, on the first line of main(), not on a request from a real user hours
// later. A stream gateway that silently defaulted its authorizer would be a vulnerability with a
// changelog entry.
type Options struct {
	// Principal answers "who is calling?" from the request — a session cookie, an mTLS peer, a
	// header your ingress set. Return an error to refuse.
	//
	// Required, and there is deliberately no default. Every default that could go here ("nil
	// principal", "the request's Authorization header") is a policy decision about someone's auth
	// system, and this library does not get to make one.
	Principal func(*http.Request) (Principal, error)

	// Authorizer decides whether that principal may open this scope, BEFORE any watch is opened.
	// Required. gateway.AllowAll{} exists, is tedious to type on purpose, and shows up in a diff.
	Authorizer Authorizer

	// Clients resolves (target, principal) to an upstream. Required.
	Clients ClientFor

	// Scopes is the allowlist (spec §8). Required, and deny-by-default: the zero value streams
	// nothing, so forgetting it fails closed.
	Scopes ScopePolicy

	// Projection defaults to ProjectionEditor — the safe one. A gateway that defaulted to raw and
	// streamed Secret values because someone omitted a line would have a vulnerability, not a bug.
	Projection Projection

	// Ordering defaults to OrderingStrict (Kubernetes 1.35+ conformance). See stream.go.
	Ordering ResourceVersionOrdering
}

// Handler mounts the stream on one route.
//
// Every rejection — a malformed scope, a disallowed one, an unidentifiable caller — is delivered as
// a TERMINAL SSE error event over a 200, not as an HTTP status code. That is not sloppiness, it is
// the only thing that works: a browser's EventSource cannot read the body of a non-200, so a 403
// reaches the page as an `onerror` with no detail and no reason, and the developer is left guessing.
// A terminal error event carries a code and a message the UI can actually show, and `terminal` is
// what stops EventSource from reconnecting forever.
func Handler(o Options) http.Handler {
	switch {
	case o.Principal == nil:
		panic("krm-stream: Options.Principal is required — the library must never assume who the caller is")
	case o.Authorizer == nil:
		panic("krm-stream: Options.Authorizer is required — use gateway.AllowAll{} to say you meant it")
	case o.Clients == nil:
		panic("krm-stream: Options.Clients is required — the library holds no cluster connection of its own")
	}

	g := &Gateway{
		Auth:       o.Authorizer,
		Clients:    o.Clients,
		Projection: o.Projection,
		Ordering:   o.Ordering,
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scope, serr := ScopeFromQuery(r.URL.Query())
		if serr == nil {
			serr = o.Scopes.Validate(scope)
		}
		if serr != nil {
			refuse(w, serr)
			return
		}

		principal, err := o.Principal(r)
		if err != nil {
			// Forbidden, not the host's error verbatim: whatever went wrong identifying this caller
			// is the host's business and possibly its internals. The caller learns that they may not
			// have this, and nothing else.
			refuse(w, Forbidden("not authenticated"))
			return
		}

		g.ServeStream(w, r, principal, scope)
	})
}

// refuse writes a terminal error as a well-formed one-event stream, and closes.
func refuse(w http.ResponseWriter, serr *StreamError) {
	WriteSSEHeaders(w)
	_ = NewSSESink(w).Emit(context.Background(), serr.Event())
}
