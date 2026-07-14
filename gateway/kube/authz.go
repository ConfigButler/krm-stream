package kube

import (
	"context"
	"fmt"

	authzv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/ConfigButler/krm-stream/gateway"
)

// Keeping Kubernetes as the authorization boundary, even when the watch is shared.
//
// The library's whole stance is that it does not authorize — ClientFor opens the upstream watch AS
// the caller, so the API server's own RBAC decides, and no bug in this library can talk it round
// (docs/auth.md).
//
// SharedBackend is the one place that breaks. A shared watch is opened ONCE, so it is opened as ONE
// identity — your service account — and every subscriber then reads from its cache. At that moment
// the Authorizer stops being defence in depth and becomes the only thing between a caller and the
// objects. The honest thing to do about that is not to write a warning label. It is to keep asking
// Kubernetes.
//
// So: before a subscriber is served from a shared cache, ASK THE API SERVER — "may this user watch
// this resource, in this namespace?" — with a SubjectAccessReview, and let it answer. The boundary is
// Kubernetes' again. That is the point:
//
//	shared := gateway.NewSharedBackend(serviceAccountBackend)   // one watch, one identity
//	opts.Authorizer = kube.SSARAuthorizer(clientset, subjectOf) // …but RBAC still decides
//	opts.Clients    = func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return shared, nil }
//
// Because the gateway re-authorizes on every snapshot cycle (stream.go), this is also how a
// revocation reaches a stream that is already open.

// Subject is a caller as KUBERNETES understands one. The library's Principal is deliberately opaque —
// it is whatever your session says a user is — so translating it into a Kubernetes subject is a thing
// only the host can do, and this is where it does it.
type Subject struct {
	// User is the username RBAC binds against (the OIDC `username` claim, typically).
	User string
	// Groups are the groups RBAC binds against (the OIDC `groups` claim, typically).
	Groups []string
	// UID and Extra are optional, and are carried through verbatim for audit and for authorizers
	// (webhook, OPA) that key on them.
	UID   string
	Extra map[string]authzv1.ExtraValue
}

// SubjectFor maps a Principal onto a Kubernetes subject. Return an error to refuse the caller.
type SubjectFor func(gateway.Principal) (Subject, error)

// SSARAuthorizer authorizes a scope by asking the API server, with a SubjectAccessReview, whether the
// caller may `list` and `watch` that resource.
//
// BOTH verbs, and that is not belt-and-braces: a snapshot cycle is a list followed by a watch — quite
// literally so on the §3b path, where the gateway issues a real LIST — so a caller who may watch but
// not list can still be served objects by the list. Checking only `watch` would authorize half of
// what we are about to do.
//
// It needs the SERVER's own client (a service account) to hold `create` on `subjectaccessreviews`,
// which is the standard `system:auth-delegator` role. It does NOT need impersonate rights: this asks
// a question about a user, it does not act as one.
func SSARAuthorizer(cs kubernetes.Interface, subjectFor SubjectFor) gateway.Authorizer {
	return gateway.AuthorizerFunc(func(ctx context.Context, p gateway.Principal, scope gateway.Scope) error {
		subject, err := subjectFor(p)
		if err != nil {
			return gateway.Forbidden("not authenticated")
		}
		if subject.User == "" && len(subject.Groups) == 0 {
			// An empty subject is not "anonymous, and therefore probably fine" — it is a request that
			// RBAC would evaluate against nobody, and whose result would be meaningless. Refuse.
			return gateway.Forbidden("no Kubernetes subject for this caller")
		}

		for _, verb := range []string{"list", "watch"} {
			allowed, reason, err := review(ctx, cs, subject, scope, verb)
			if err != nil {
				// A SubjectAccessReview we could not complete is NOT an allow. If the API server
				// cannot tell us whether this caller may look, the answer is no.
				return fmt.Errorf("krm-stream/kube: subject access review (%s): %w", verb, err)
			}
			if !allowed {
				// The API server's own words, so an operator can find this decision in the audit log
				// rather than guessing at ours.
				return gateway.Forbidden(fmt.Sprintf("Kubernetes refused %q on %s for %s: %s",
					verb, groupResource(scope), subject.User, reason))
			}
		}
		return nil
	})
}

func review(ctx context.Context, cs kubernetes.Interface, s Subject, scope gateway.Scope, verb string) (bool, string, error) {
	sar := &authzv1.SubjectAccessReview{
		Spec: authzv1.SubjectAccessReviewSpec{
			User:   s.User,
			Groups: s.Groups,
			UID:    s.UID,
			Extra:  s.Extra,
			ResourceAttributes: &authzv1.ResourceAttributes{
				Verb:      verb,
				Group:     scope.Group,
				Version:   scope.Version,
				Resource:  scope.Resource,
				Namespace: scope.Namespace,
				// Name is set for a single-object scope: RBAC can grant `get`/`watch` on ONE named
				// object, and a scope that names one is exactly that case. Leaving it empty would ask
				// a broader question than the one we are about to act on, and would refuse a caller
				// who is legitimately allowed the narrow thing.
				Name: scope.Name,
			},
		},
	}

	got, err := cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, sar, metav1.CreateOptions{})
	if err != nil {
		return false, "", err
	}
	reason := got.Status.Reason
	if got.Status.EvaluationError != "" {
		reason = got.Status.EvaluationError
	}
	return got.Status.Allowed && !got.Status.Denied, reason, nil
}

func groupResource(s gateway.Scope) string {
	if s.Group == "" {
		return s.Resource
	}
	return s.Group + "/" + s.Resource
}
