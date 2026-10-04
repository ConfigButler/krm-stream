package kube_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	authzv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestSubjectAccessReviewAsksKubernetesTheRightQuestion(t *testing.T) {
	cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })

	err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)
	if err != nil {
		t.Fatalf("an allowed caller was refused: %v", err)
	}

	// BOTH verbs. A snapshot cycle is a list THEN a watch — literally so on the list-then-watch path, where the
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
func TestSubjectAccessReviewRefusalIsTerminalForbidden(t *testing.T) {
	cs, _ := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return false, false })

	err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)

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

	if err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope); err == nil {
		t.Fatal("a caller who may watch but NOT list was authorized — the snapshot would enumerate " +
			"objects RBAC just refused to let them enumerate")
	}
}

// An explicit Denied (a webhook authorizer saying "no", as opposed to "no opinion") must not be
// overridden by an Allowed elsewhere in the status.
func TestAnExplicitDenyWins(t *testing.T) {
	cs, _ := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, true })

	if err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope); err == nil {
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

	err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)
	if err == nil {
		t.Fatal("a SubjectAccessReview that FAILED was treated as an allow — if the API server cannot " +
			"tell us whether this caller may look, the answer is no")
	}
}

// A principal the host cannot map to a Kubernetes subject is not "anonymous, and therefore probably
// fine". RBAC would evaluate it against nobody, and the result would be meaningless.
func TestAnUnmappablePrincipalIsRefused(t *testing.T) {
	cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })

	err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(context.Background(), "not-a-user", configmapScope)
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
	if err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, scope); err != nil {
		t.Fatalf("Authorize: %v", err)
	}

	for _, sar := range *asked {
		if got := sar.Spec.ResourceAttributes.Name; got != "app-config" {
			t.Errorf("%s asked about name %q, want app-config", sar.Spec.ResourceAttributes.Verb, got)
		}
	}
}

// A review the API server throttles is retryable, and keeps the wait the API server asked for, all
// the way to the event the browser reads.
func TestAThrottledReviewKeepsItsRetryHint(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "subjectaccessreviews",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewTooManyRequests("priority and fairness", 7)
		})
	handler := gateway.Handler(gateway.Options{
		Principal: func(*http.Request) (gateway.Principal, error) { return alice, nil },
		Scopes:    gateway.ScopePolicy{Targets: []string{""}, AnyResource: true},
		StreamConfig: gateway.StreamConfig{
			Authorizer: kube.SubjectAccessReviewAuthorizer(cs, subjectOf),
			Clients: func(context.Context, string, gateway.Principal) (gateway.Backend, error) {
				t.Fatal("a throttled review opened a watch")
				return nil, nil
			},
		},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s?version=v1&resource=configmaps&namespace=app", nil))

	body := rec.Body.String()
	for _, want := range []string{`"code":"UPSTREAM_UNAVAILABLE"`, `"retryAfterMs":7000`, `"terminal":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("want %s in:\n%s", want, body)
		}
	}
}

// The review's inputs are a contract: a host that caches decisions keys on exactly them, and a later
// version that sent another attribute — a label selector, say — would make such a key wrong. So this
// pins the WHOLE request, field by field, for a named scope and for a scope carrying a selector. A
// change here is a change hosts must hear about in the release notes.
func TestSubjectAccessReviewSendsExactlyTheDocumentedAttributes(t *testing.T) {
	subject := kube.Subject{
		User:   "alice@example.com",
		Groups: []string{"devs", "system:authenticated"},
		UID:    "4b1d-alice",
		Extra:  map[string]authzv1.ExtraValue{"scopes.example.com/tenant": {"blue"}},
	}
	subjectFor := func(gateway.Principal) (kube.Subject, error) { return subject, nil }

	for name, tc := range map[string]struct {
		scope gateway.Scope
		want  authzv1.ResourceAttributes // without its verb
	}{
		"a named object in a group": {
			scope: gateway.Scope{Target: "production", Group: "apps", Version: "v1", Resource: "deployments", Namespace: "app", Name: "web"},
			want:  authzv1.ResourceAttributes{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "app", Name: "web"},
		},
		"a collection narrowed by a label selector": {
			scope: gateway.Scope{Target: "production", Version: "v1", Resource: "configmaps", Namespace: "app", LabelSelector: "tier=web"},
			// Selectors are omitted from the authorization request.
			want: authzv1.ResourceAttributes{Version: "v1", Resource: "configmaps", Namespace: "app"},
		},
		"every namespace": {
			scope: gateway.Scope{Version: "v1", Resource: "configmaps"},
			want:  authzv1.ResourceAttributes{Version: "v1", Resource: "configmaps"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			cs, asked := reviewer(func(*authzv1.SubjectAccessReview) (bool, bool) { return true, false })
			if err := kube.SubjectAccessReviewAuthorizer(cs, subjectFor).Authorize(t.Context(), "alice", tc.scope); err != nil {
				t.Fatalf("Authorize: %v", err)
			}
			if len(*asked) != 2 {
				t.Fatalf("asked %d reviews, want exactly two: list, then watch", len(*asked))
			}
			for i, verb := range []string{"list", "watch"} {
				ra := tc.want
				ra.Verb = verb
				want := authzv1.SubjectAccessReviewSpec{
					User: subject.User, Groups: subject.Groups, UID: subject.UID, Extra: subject.Extra,
					ResourceAttributes: &ra,
				}
				got := (*asked)[i].Spec
				if !reflect.DeepEqual(got, want) {
					t.Errorf("review %d:\n got %+v\nwant %+v", i, got, want)
				}
				// Spelled out, so the omissions read as deliberate rather than incidental.
				if got.ResourceAttributes.LabelSelector != nil || got.ResourceAttributes.FieldSelector != nil {
					t.Errorf("review %d carried a selector: %+v", i, got.ResourceAttributes)
				}
				if got.ResourceAttributes.Subresource != "" || got.NonResourceAttributes != nil {
					t.Errorf("review %d asked about a subresource or a non-resource URL", i)
				}
			}
		})
	}
}

// The adapter stops at the first refusal or failure: a caller refused `list` is never asked about
// `watch`, and the error names the refused verb.
func TestSubjectAccessReviewStopsAtTheFirstRefusal(t *testing.T) {
	for name, fail := range map[string]func(*authzv1.SubjectAccessReview) (bool, bool, error){
		"refused": func(*authzv1.SubjectAccessReview) (bool, bool, error) { return false, false, nil },
		"failed":  func(*authzv1.SubjectAccessReview) (bool, bool, error) { return false, false, errors.New("unreachable") },
	} {
		t.Run(name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			var verbs []string
			cs.PrependReactor("create", "subjectaccessreviews",
				func(action k8stesting.Action) (bool, runtime.Object, error) {
					sar, _ := action.(k8stesting.CreateAction).GetObject().(*authzv1.SubjectAccessReview)
					verbs = append(verbs, sar.Spec.ResourceAttributes.Verb)
					allowed, denied, err := fail(sar)
					if err != nil {
						return true, nil, err
					}
					sar.Status = authzv1.SubjectAccessReviewStatus{Allowed: allowed, Denied: denied}
					return true, sar, nil
				})
			err := kube.SubjectAccessReviewAuthorizer(cs, subjectOf).Authorize(t.Context(), alice, configmapScope)
			if err == nil || !strings.Contains(err.Error(), "list") {
				t.Fatalf("err = %v, want a refusal naming list", err)
			}
			if !reflect.DeepEqual(verbs, []string{"list"}) {
				t.Errorf("asked about %v, want only list", verbs)
			}
		})
	}
}
