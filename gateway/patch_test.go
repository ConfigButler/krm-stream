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

func TestValidateNativeMergePatchRequiresPreconditionsAndProtectsMachinery(t *testing.T) {
	const conditional = `"uid":"u1","resourceVersion":"7"`
	for _, patch := range []string{
		`{"metadata":{` + conditional + `},"data":{"mode":"b"}}`,
		`{"metadata":{` + conditional + `,"annotations":{"owner":"team-b","gone":null}},"spec":{"replicas":2}}`,
		`{"metadata":{` + conditional + `,"labels":null}}`,
	} {
		if err := ValidateNativeMergePatch([]byte(patch)); err != nil {
			t.Errorf("patch %s rejected: %v", patch, err)
		}
	}

	for _, patch := range []string{
		`{"data":{"mode":"b"}}`,
		`{"metadata":{"uid":"u1"},"data":{"mode":"b"}}`,
		`{"metadata":{"resourceVersion":"7"},"data":{"mode":"b"}}`,
		`{"metadata":{"uid":"","resourceVersion":"7"}}`,
		`{"metadata":{"uid":"u1","resourceVersion":7}}`,
		`{"metadata":null}`,
		`null`, `[]`, `{`,
	} {
		if err := ValidateNativeMergePatch([]byte(patch)); err == nil {
			t.Errorf("unconditional or malformed patch %s was accepted", patch)
		}
	}

	for _, patch := range []string{
		`{"metadata":{` + conditional + `,"managedFields":[]}}`,
		`{"metadata":{` + conditional + `,"managedFields":null}}`,
		`{"metadata":{` + conditional + `,"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{}"}}}`,
		`{"metadata":{` + conditional + `,"annotations":null}}`,
	} {
		var violation *PatchViolation
		if err := ValidateNativeMergePatch([]byte(patch)); !errors.As(err, &violation) {
			t.Errorf("patch %s: want PatchViolation, got %v", patch, err)
		}
	}
}

// Project is exported because the SAVE path needs it, and until it was, a host answering a save with
// the written object had no supported way to make that object safe. The obvious thing — hand back
// what the Kubernetes client returned — leaks exactly what the stream withholds, through the one
// endpoint the stream does not guard.
//
// So this asserts the property a host is relying on: what Project returns is safe to send to a
// browser, and it names what it withheld.
func TestProjectMakesASavedObjectSafeToReturn(t *testing.T) {
	// What a Kubernetes write actually hands back.
	raw := KRMObject{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"uid":  "u-1",
			"name": "creds",
			"managedFields": []any{
				map[string]any{"manager": "kubectl"},
			},
			"annotations": map[string]any{
				lastAppliedAnnotation: `{"apiVersion":"v1"}`,
			},
		},
		"data": map[string]any{"token": "s3cret"},
	}

	projected, redacted := Project(ProjectionFull, raw)

	meta, _ := projected["metadata"].(map[string]any)
	if _, found := meta["managedFields"]; found {
		t.Error("managedFields survived Project — a save response would put it in the browser")
	}
	if ann, found := meta["annotations"].(map[string]any); found {
		if _, found := ann[lastAppliedAnnotation]; found {
			t.Error("last-applied-configuration survived Project")
		}
	}

	data, _ := projected["data"].(map[string]any)
	if got, found := data["token"]; found {
		t.Errorf("the Secret VALUE survived Project: %v — this is the leak the projection exists to prevent", got)
	}
	if len(redacted) != 1 || redacted[0] != "/data/token" {
		t.Errorf("redacted = %v, want [/data/token] — a consumer must still learn the key exists", redacted)
	}

	// And the input is untouched: the caller's object may be an informer's, shared with every other
	// stream on the same scope.
	if raw["data"].(map[string]any)["token"] != "s3cret" {
		t.Error("Project mutated its input")
	}
}
