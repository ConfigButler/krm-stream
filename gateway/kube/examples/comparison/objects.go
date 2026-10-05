package comparison

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// The objects every source watches: a few Widgets — a CRD with spec and a REAL status subresource, so
// a controller-style status write moves resourceVersion without touching spec — and a few Secrets.
//
// The Widget definition is the one gateway/kube/composition_e2e_test.go installs, copied rather than
// imported (test code is not a library). Both create it only when it is missing and never delete it:
// it is cluster-scoped and identical, so the real-API suite and this host can share a cluster.

// WidgetGroup is the Widget CRD's API group.
const WidgetGroup = "e2e.krm-stream.configbutler.io"

var (
	widgets = schema.GroupVersionResource{Group: WidgetGroup, Version: "v1", Resource: "widgets"}
	secrets = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	crds    = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// ensureWidgetCRD installs the Widget CRD when it is missing and waits until it is served.
func ensureWidgetCRD(ctx context.Context, dyn dynamic.Interface) error {
	preserve := map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": widgets.Resource + "." + widgets.Group},
		"spec": map[string]any{
			"group": widgets.Group,
			"scope": "Namespaced",
			"names": map[string]any{"plural": "widgets", "singular": "widget", "kind": "Widget", "listKind": "WidgetList"},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"subresources": map[string]any{"status": map[string]any{}},
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"spec": preserve, "status": preserve},
				}},
			}},
		},
	}}
	if _, err := dyn.Resource(crds).Create(ctx, crd, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("install the Widget CRD: %w", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := dyn.Resource(widgets).Namespace("default").List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the Widget CRD is not served: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// WidgetName and SecretName name the fixed objects.
func WidgetName(i int) string { return fmt.Sprintf("w-%02d", i) }

// SecretName names the i-th Secret.
func SecretName(i int) string { return fmt.Sprintf("s-%d", i) }

var colors = []string{"red", "green", "blue", "amber"}

func initialSpec(i int) map[string]any {
	return map[string]any{"replicas": int64(i%5 + 1), "color": colors[i%len(colors)], "note": "initial", "revision": int64(0)}
}

func initialStatus() map[string]any {
	return map[string]any{"phase": "Ready", "heartbeat": int64(0), "observedRevision": int64(0)}
}

func initialSecretData(i int) map[string]any {
	return map[string]any{
		"token": base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "initial-token-%d", i)),
		"user":  base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "svc-%d", i)),
	}
}

// objects is the fixed set in one namespace, written with the host's own client.
type objects struct {
	dyn       dynamic.Interface
	namespace string
	widgets   int
	secrets   int
}

func (o *objects) widgetClient() dynamic.ResourceInterface {
	return o.dyn.Resource(widgets).Namespace(o.namespace)
}

func (o *objects) secretClient() dynamic.ResourceInterface {
	return o.dyn.Resource(secrets).Namespace(o.namespace)
}

// create makes every object, with its initial status written through the status subresource.
func (o *objects) create(ctx context.Context) error {
	for i := range o.widgets {
		w := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": widgets.GroupVersion().String(),
			"kind":       "Widget",
			"metadata":   map[string]any{"name": WidgetName(i), "labels": map[string]any{"compare": "widget"}},
			"spec":       initialSpec(i),
		}}
		if _, err := o.widgetClient().Create(ctx, w, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create widget %s: %w", WidgetName(i), err)
		}
	}
	for i := range o.secrets {
		s := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": SecretName(i), "labels": map[string]any{"compare": "secret"}},
			"type":       "Opaque",
			"data":       initialSecretData(i),
		}}
		if _, err := o.secretClient().Create(ctx, s, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create secret %s: %w", SecretName(i), err)
		}
	}
	return o.reset(ctx)
}

// reset writes every object back to its initial content, so each measurement starts from the same
// state. Spec and data are replaced; status goes through its subresource.
func (o *objects) reset(ctx context.Context) error {
	for i := range o.widgets {
		if err := o.patch(ctx, o.widgetClient(), WidgetName(i), map[string]any{"spec": initialSpec(i)}); err != nil {
			return err
		}
		if err := o.patch(ctx, o.widgetClient(), WidgetName(i), map[string]any{"status": initialStatus()}, "status"); err != nil {
			return err
		}
	}
	for i := range o.secrets {
		if err := o.patch(ctx, o.secretClient(), SecretName(i), map[string]any{"data": initialSecretData(i)}); err != nil {
			return err
		}
	}
	return nil
}

func (o *objects) patch(ctx context.Context, client dynamic.ResourceInterface, name string, body map[string]any, subresource ...string) error {
	patch, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := client.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}, subresource...); err != nil {
		return fmt.Errorf("patch %s %v: %w", name, subresource, err)
	}
	return nil
}

// StateObject is one object as the cluster holds it, for the measurement's correctness gate.
type StateObject struct {
	Kind            string            `json:"kind"`
	UID             string            `json:"uid"`
	Name            string            `json:"name"`
	ResourceVersion string            `json:"resourceVersion"`
	Labels          map[string]string `json:"labels,omitempty"`
	Spec            any               `json:"spec,omitempty"`
	Status          any               `json:"status,omitempty"`
	Data            map[string]any    `json:"data,omitempty"`
}

// state lists every object with a consistent read.
func (o *objects) state(ctx context.Context) ([]StateObject, error) {
	var out []StateObject
	for _, client := range []dynamic.ResourceInterface{o.widgetClient(), o.secretClient()} {
		list, err := client.List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for _, item := range list.Items {
			data, _, _ := unstructured.NestedMap(item.Object, "data")
			out = append(out, StateObject{
				Kind:            item.GetKind(),
				UID:             string(item.GetUID()),
				Name:            item.GetName(),
				ResourceVersion: item.GetResourceVersion(),
				Labels:          item.GetLabels(),
				Spec:            item.Object["spec"],
				Status:          item.Object["status"],
				Data:            data,
			})
		}
	}
	return out, nil
}
