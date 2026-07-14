package gateway

import "testing"

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
