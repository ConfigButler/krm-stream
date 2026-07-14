package gateway

import (
	"errors"
	"testing"
)

func TestValidateMergePatchAllowsOnlyVisibleEditableChanges(t *testing.T) {
	secret := KRMObject{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"uid": "s1", "name": "credentials", "labels": map[string]any{"app": "old"}},
		"data":     map[string]any{"sample": "example"},
	}

	if err := ValidateMergePatch(ProjectionFull, secret, []byte(`{"metadata":{"labels":{"app":"new"}}}`)); err != nil {
		t.Fatalf("visible label patch rejected: %v", err)
	}

	for _, patch := range []string{
		`{"data":{"sample":"overwrite"}}`,
		`{"data":null}`,
		`{"metadata":{"managedFields":[]}}`,
		`{"metadata":{"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}"}}}`,
	} {
		err := ValidateMergePatch(ProjectionFull, secret, []byte(patch))
		var violation *PatchViolation
		if !errors.As(err, &violation) {
			t.Errorf("patch %s: want PatchViolation, got %v", patch, err)
		}
	}
}

func TestValidateMergePatchHonorsProjectionAndPatchShape(t *testing.T) {
	deployment := KRMObject{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"uid": "d1", "name": "web"},
		"spec":     map[string]any{"replicas": 1},
		"status":   map[string]any{"readyReplicas": 1},
	}

	if err := ValidateMergePatch(ProjectionSpec, deployment, []byte(`{"spec":{"replicas":2}}`)); err != nil {
		t.Fatalf("visible spec patch rejected: %v", err)
	}
	if err := ValidateMergePatch(ProjectionSpec, deployment, []byte(`{"status":null}`)); err == nil {
		t.Fatal("status patch under krm-spec/v1 was accepted")
	}
	if err := ValidateMergePatch(ProjectionRaw, deployment, []byte(`{"status":{"readyReplicas":2}}`)); err != nil {
		t.Fatalf("raw projection unexpectedly rejected status patch: %v", err)
	}
	for _, patch := range []string{`null`, `[]`, `{`} {
		if err := ValidateMergePatch(ProjectionFull, deployment, []byte(patch)); err == nil {
			t.Errorf("invalid merge patch %q was accepted", patch)
		}
	}
}
