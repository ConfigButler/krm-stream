package gateway

import "context"

// ProjectionPolicy chooses the effective projection for one principal and scope. The caller may ask
// for a name, but only the host may grant it: a projection is a disclosure policy, not a browser
// preference. The returned value is echoed in reset and is the only projection the gateway applies.
type ProjectionPolicy interface {
	SelectProjection(context.Context, Principal, Scope, Projection) (Projection, error)
}

// ProjectionPolicyFunc adapts a function for small hosts and tests.
type ProjectionPolicyFunc func(context.Context, Principal, Scope, Projection) (Projection, error)

// SelectProjection calls f.
func (f ProjectionPolicyFunc) SelectProjection(ctx context.Context, principal Principal, scope Scope, requested Projection) (Projection, error) {
	return f(ctx, principal, scope, requested)
}

// StaticProjection grants exactly one projection. It is the safe default when a host has no need to
// offer a choice, and refuses an attempt to request any other named view.
type StaticProjection Projection

// SelectProjection grants only p.
func (p StaticProjection) SelectProjection(_ context.Context, _ Principal, _ Scope, requested Projection) (Projection, error) {
	effective := Projection(p)
	if effective == "" {
		effective = ProjectionFull
	}
	if requested != "" && requested != effective {
		return "", Forbidden("requested projection is not permitted: " + string(requested))
	}
	return effective, nil
}
