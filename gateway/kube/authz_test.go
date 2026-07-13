package kube_test

import (
	"context"
	"errors"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
)

// An authorizer is the one component where a bug is a disclosure, so these tests are mostly about the
// ways it must say NO — including the ways that are easy to get accidentally permissive.

type user struct {
	name   string
	groups []string
}

func subjectOf(p gateway.Principal) (kube.Subject, error) {
	u, ok := p.(*user)
	if !ok {
		return kube.Subject{}, errors.New("not a user")
	}
	return kube.Subject{User: u.name, Groups: u.groups}, nil
}

var alice = &user{name: "alice@example.com", groups: []string{"devs"}}

var configmapScope = gateway.Scope{Version: "v1", Resource: "configmaps", Namespace: "app"}

// reviewer builds a fake API server that answers SubjectAccessReviews with `decide`, and records
// every question it was asked.
func reviewer(decide func(*authzv1.SubjectAccessReview) (allowed bool, denied bool)) (*fake.Clientset, *[]*authzv1.SubjectAccessReview) {
	cs := fake.NewSimpleClientset()
	var asked []*authzv1.SubjectAccessReview

	cs.PrependReactor("create", "subjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar, _ := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
			asked = append(asked, sar)
			allowed, denied := decide(sar)
			sar.Status = authzv1.SubjectAccessReviewStatus{
				Allowed: allowed, Denied: denied, Reason: "because the fake said so",
			}
			return true, sar, nil
		})
	return cs, &asked
}

func TestSSARAsksKubernetesTheRightQuestion(t *testing.T) {
	cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })

	err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)
	if err != nil {
		t.Fatalf("an allowed caller was refused: %v", err)
	}

	// BOTH verbs. A snapshot cycle is a list THEN a watch — literally so on the §3b path, where the
	// gateway issues a real LIST — so a caller who may watch but not list can still be handed objects
	// by the list. Checking only `watch` authorizes half of what we are about to do.
	verbs := map[string]bool{}
	for _, sar := range *asked {
		verbs[sar.Spec.ResourceAttributes.Verb] = true
	}
	if !verbs["list"] || !verbs["watch"] {
		t.Errorf("asked about %v, want both list and watch", verbs)
	}

	got := (*asked)[0].Spec
	if got.User != alice.name || len(got.Groups) != 1 || got.Groups[0] != "devs" {
		t.Errorf("subject = %q %v, want alice and her groups — RBAC binds against these", got.User, got.Groups)
	}
	if ra := got.ResourceAttributes; ra.Resource != "configmaps" || ra.Namespace != "app" || ra.Version != "v1" {
		t.Errorf("resource attributes = %+v, want the scope we are about to open", ra)
	}
}

// The whole point: Kubernetes says no, so we say no.
func TestSSARRefusalIsTerminalForbidden(t *testing.T) {
	cs, _ := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return false, false })

	err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)

	var se *gateway.StreamError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v (%T), want a *gateway.StreamError", err, err)
	}
	if se.Code != gateway.CodeForbidden {
		t.Errorf("code = %v, want FORBIDDEN", se.Code)
	}
	if !se.Terminal {
		t.Error("a refusal must be TERMINAL: EventSource reconnects on its own, so a user who may " +
			"never see this scope would hammer it forever")
	}
}

// A caller may be allowed to `watch` and NOT to `list`. The snapshot lists. Serving them anyway would
// hand them, in the snapshot, exactly the objects RBAC just said they could not enumerate.
func TestWatchWithoutListIsRefused(t *testing.T) {
	cs, _ := reviewer(func(sar *authzv1.SubjectAccessReview) (bool, bool) {
		return sar.Spec.ResourceAttributes.Verb == "watch", false // list: denied
	})

	if err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope); err == nil {
		t.Fatal("a caller who may watch but NOT list was authorized — the snapshot would enumerate " +
			"objects RBAC just refused to let them enumerate")
	}
}

// An explicit Denied (a webhook authorizer saying "no", as opposed to "no opinion") must not be
// overridden by an Allowed elsewhere in the status.
func TestAnExplicitDenyWins(t *testing.T) {
	cs, _ := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, true })

	if err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope); err == nil {
		t.Fatal("Status.Denied was ignored: an authorizer that explicitly DENIED this caller was overruled")
	}
}

// The one that would be a disclosure: if we cannot ask, we must not assume.
func TestAFailedReviewIsNotAnAllow(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "subjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("the API server is unreachable")
		})

	err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)
	if err == nil {
		t.Fatal("a SubjectAccessReview that FAILED was treated as an allow — if the API server cannot " +
			"tell us whether this caller may look, the answer is no")
	}
}

// A principal the host cannot map to a Kubernetes subject is not "anonymous, and therefore probably
// fine". RBAC would evaluate it against nobody, and the result would be meaningless.
func TestAnUnmappablePrincipalIsRefused(t *testing.T) {
	cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })

	err := kube.SSARAuthorizer(cs, subjectOf).Authorize(context.Background(), "not-a-user", configmapScope)
	if err == nil {
		t.Fatal("a principal with no Kubernetes subject was authorized")
	}
	if len(*asked) != 0 {
		t.Error("we asked the API server about a subject we could not even name")
	}
}

// A named scope asks the NARROW question. RBAC can grant a verb on one named object, and a scope that
// names one is exactly that case — asking the broader question would refuse a caller who is
// legitimately allowed the narrow thing.
func TestANamedScopeAsksAboutThatName(t *testing.T) {
	cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })

	scope := configmapScope
	scope.Name = "app-config"
	if err := kube.SSARAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, scope); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	if got := (*asked)[0].Spec.ResourceAttributes.Name; got != "app-config" {
		t.Errorf("asked about name %q, want app-config", got)
	}
}
