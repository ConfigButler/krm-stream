package gateway

import (
	"net/url"
	"testing"
)

// The typed-nil trap, pinned shut — in the exact shape that bit the first host to adopt this.
//
// Validate and ScopeFromQuery must return `error`, not `*StreamError`. If either ever goes back to
// the concrete pointer type, THIS TEST STILL COMPILES and starts failing, because assigning a nil
// *StreamError into an `error` produces a non-nil interface. That is the whole bug: the gateway says
// yes and the caller reads no.
//
// It fails safe here (a refusal nobody expected) and it fails safe in an authorizer written the same
// way (everyone refused, someone notices in a minute). Invert the check — `if err == nil { serve() }`
// — and the identical bug admits everyone, silently. That asymmetry is why this is a test and not a
// doc comment.
func TestSuccessIsNilThroughTheErrorInterface(t *testing.T) {
	policy := ScopePolicy{
		Targets:   []string{"demo"},
		Resources: []GroupResource{{Resource: "configmaps", Scope: ResourceScopeNamespaced}},
	}
	scope := Scope{Target: "demo", Version: "v1", Resource: "configmaps", Namespace: "app"}

	// Declared as `error` FIRST, then assigned — which is what a host does, and what makes a concrete
	// return type dangerous. `err := policy.Validate(...)` would hide this by inferring *StreamError.
	var err error

	if err = policy.Validate(scope); err != nil {
		t.Fatalf("a valid scope came back non-nil through an error interface: %v — the typed-nil trap is back", err)
	}

	q := url.Values{"target": {"demo"}, "version": {"v1"}, "resource": {"configmaps"}, "namespace": {"app"}}
	if _, err = ScopeFromQuery(q); err != nil {
		t.Fatalf("a valid query came back non-nil through an error interface: %v — the typed-nil trap is back", err)
	}
}

func TestScopePolicyRequiresExplicitNamespaceSemantics(t *testing.T) {
	policy := ScopePolicy{
		Targets: []string{"demo"},
		Resources: []GroupResource{
			{Resource: "configmaps", Scope: ResourceScopeNamespaced},
			{Resource: "nodes", Scope: ResourceScopeCluster},
		},
	}

	for _, tc := range []struct {
		name  string
		scope Scope
		ok    bool
	}{
		{"one namespace", Scope{Target: "demo", Resource: "configmaps", Namespace: "app"}, true},
		{"namespaced resource without namespace", Scope{Target: "demo", Resource: "configmaps"}, false},
		{"cluster resource without namespace", Scope{Target: "demo", Resource: "nodes"}, true},
		{"cluster resource with namespace", Scope{Target: "demo", Resource: "nodes", Namespace: "app"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := policy.Validate(tc.scope)
			if (err == nil) != tc.ok {
				t.Fatalf("Validate(%+v) = %v, want allowed=%t", tc.scope, err, tc.ok)
			}
		})
	}

	policy.Resources[0].AllowAllNamespaces = true
	if err := policy.Validate(Scope{Target: "demo", Resource: "configmaps"}); err != nil {
		t.Fatalf("explicit all-namespaces policy was rejected: %v", err)
	}
}

func TestScopePolicyRejectsSelectorsUnlessEnabled(t *testing.T) {
	policy := ScopePolicy{
		Targets:   []string{"demo"},
		Resources: []GroupResource{{Resource: "configmaps", Scope: ResourceScopeNamespaced}},
	}
	scope := Scope{Target: "demo", Resource: "configmaps", Namespace: "app", LabelSelector: "app=shop"}
	if err := policy.Validate(scope); err == nil {
		t.Fatal("label selector was accepted without explicit policy")
	}
	policy.AllowLabelSelector = true
	if err := policy.Validate(scope); err != nil {
		t.Fatalf("enabled label selector was rejected: %v", err)
	}
}
