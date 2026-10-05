package comparison

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// The measurement driver rejects a run whose workload did not do what it planned, and requires every
// projected subscriber to hold the version of each Secret's FINAL successful rotation. Both rely on
// what run reports, so a failed write must show up as an error and a missing `done`, and only a
// successful rotation may set the version.
func TestRunReportsFailuresAndTheLastSuccessfulRotation(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		widgets: "WidgetList",
		secrets: "SecretList",
	})
	// The rotations' answers in order: s-0 ok, s-1 fails, s-1 ok, s-0 fails.
	versions := []string{"11", "", "13", ""}
	rotation := 0
	client.PrependReactor("patch", "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
		rv := versions[rotation]
		rotation++
		if rv == "" {
			return true, nil, errors.New("injected failure")
		}
		written := &unstructured.Unstructured{}
		written.SetResourceVersion(rv)
		return true, written, nil
	})
	client.PrependReactor("patch", "widgets", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{}, nil
	})

	o := &objects{dyn: client, namespace: "ns", widgets: 1, secrets: 2}
	plan := Plan{Steps: []Step{
		{Kind: StepSecret, Object: 0, Seq: 1},
		{Kind: StepSecret, Object: 1, Seq: 2},
		{Kind: StepSecret, Object: 1, Seq: 3},
		{Kind: StepSpec, Object: 0, Seq: 4},
		{Kind: StepSecret, Object: 0, Seq: 5},
	}}

	got := o.run(context.Background(), plan, func(string) int { return 0 })

	if want := map[StepKind]int{StepSecret: 4, StepSpec: 1}; !reflect.DeepEqual(got.Plan, want) {
		t.Fatalf("planned: want %v, got %v", want, got.Plan)
	}
	if want := map[StepKind]int{StepSecret: 2, StepSpec: 1}; !reflect.DeepEqual(got.Done, want) {
		t.Fatalf("done: want %v, got %v", want, got.Done)
	}
	if len(got.Errors) != 2 {
		t.Fatalf("want both failed rotations reported, got %v", got.Errors)
	}
	// s-0's last rotation failed, so its last SUCCESSFUL one (rv 11) is what a subscriber must hold.
	if want := map[string]string{SecretName(0): "11", SecretName(1): "13"}; !reflect.DeepEqual(got.SecretRotations, want) {
		t.Fatalf("rotations: want %v, got %v", want, got.SecretRotations)
	}
}
