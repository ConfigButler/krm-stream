//go:build e2e

// The real-cluster rung, for the backend itself.
//
// Everything in backend_test.go stubs the API server, and a stub is a thing I wrote: it will happily
// agree with me. This file agrees with nobody. It points the REAL stream loop at the REAL backend
// against a REAL API server and asserts the protocol comes out the other end — twice, because F6
// proved there are two ways in and a gateway that knows only one of them is broken for aggregated
// APIs.
//
//	task test-cluster
package kube_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
)

var flunders = schema.GroupVersionResource{Group: "wardle.example.com", Version: "v1alpha1", Resource: "flunders"}

// chanSink is the consumer. The gateway knows nothing about HTTP, so neither does this.
type chanSink struct{ ch chan gateway.Event }

func (s chanSink) Emit(ctx context.Context, ev gateway.Event) error {
	select {
	case s.ch <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func clients(t *testing.T) (kubernetes.Interface, dynamic.Interface) {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, nil).ClientConfig()
	if err != nil {
		t.Fatalf("kubeconfig: %v (is the cluster up? `task cluster-up`)", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("dynamic: %v", err)
	}
	return cs, dyn
}

// scratchNamespace gives each test its OWN world, and takes it away afterwards.
//
// Its own, not a shared one: a namespace deletes asynchronously, so a second test reusing the name
// races the first one's teardown and is refused ("because it is being terminated"). That is a flake
// the suite would have shipped, and it took one run against a real cluster to find.
func scratchNamespace(t *testing.T, cs kubernetes.Interface) string {
	t.Helper()
	ns := "krm-stream-e2e-" + strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	ctx := context.Background()
	_, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create namespace %s: %v", ns, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	})
	return ns
}

// stream runs a real Gateway over the real KubeBackend and hands back its events.
func stream(t *testing.T, dyn dynamic.Interface, scope gateway.Scope) <-chan gateway.Event {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	backend := kube.NewBackend(dyn)
	g := &gateway.Gateway{
		Auth:    gateway.AllowAll{},
		Clients: func(string, gateway.Principal) (gateway.Backend, error) { return backend, nil },
	}
	sink := chanSink{ch: make(chan gateway.Event, 128)}
	go func() {
		if err := g.Stream(ctx, nil, scope, sink); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("stream ended: %v", err)
		}
	}()
	return sink.ch
}

// await waits for the next event satisfying pred, failing the test if the stream goes quiet. Every
// event it skips is logged, so a failure says what DID arrive rather than merely what did not.
func await(t *testing.T, ch <-chan gateway.Event, what string, pred func(gateway.Event) bool) gateway.Event {
	t.Helper()
	deadline := time.After(90 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("the stream closed while waiting for %s", what)
			}
			if pred(ev) {
				return ev
			}
			t.Logf("  (skipped %s)", ev.Type)
		}
	}
}

func ofType(want gateway.EventType) func(gateway.Event) bool {
	return func(ev gateway.Event) bool { return ev.Type == want }
}

func named(want gateway.EventType, name string) func(gateway.Event) bool {
	return func(ev gateway.Event) bool {
		if ev.Type != want || ev.Object == nil {
			return false
		}
		meta, _ := ev.Object["metadata"].(map[string]any)
		return meta != nil && meta["name"] == name
	}
}

// §3a against kube-apiserver: the streaming list, the path F1 verified.
func TestRealClusterStreamingList(t *testing.T) {
	cs, dyn := clients(t)
	namespace := scratchNamespace(t, cs)
	ctx := context.Background()

	// Two objects in the snapshot, before the stream opens.
	for _, name := range []string{"cm-a", "cm-b"} {
		if _, err := cs.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Data:       map[string]string{"k": "v"},
		}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	ch := stream(t, dyn, gateway.Scope{
		Target: "e2e", Version: "v1", Resource: "configmaps", Namespace: namespace,
	})

	// reset … added* … synced. The snapshot is the API server's, and so is the boundary.
	await(t, ch, "reset", ofType(gateway.EventReset))
	await(t, ch, "added cm-a", named(gateway.EventAdded, "cm-a"))
	await(t, ch, "added cm-b", named(gateway.EventAdded, "cm-b"))
	await(t, ch, "synced (THE bookmark: k8s.io/initial-events-end)", ofType(gateway.EventSynced))

	// …and then it is live. Every one of these is a real API operation.
	if _, err := cs.CoreV1().ConfigMaps(namespace).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cm-c", Namespace: namespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create cm-c: %v", err)
	}
	await(t, ch, "live added cm-c", named(gateway.EventAdded, "cm-c"))

	cm, err := cs.CoreV1().ConfigMaps(namespace).Get(ctx, "cm-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm-a: %v", err)
	}
	cm.Data = map[string]string{"k": "v2"}
	if _, err := cs.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update cm-a: %v", err)
	}
	modified := await(t, ch, "live modified cm-a", named(gateway.EventModified, "cm-a"))
	if data, _ := modified.Object["data"].(map[string]any); data["k"] != "v2" {
		t.Errorf("the modified event carried data=%v, want k=v2", data)
	}

	if err := cs.CoreV1().ConfigMaps(namespace).Delete(ctx, "cm-b", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete cm-b: %v", err)
	}
	deleted := await(t, ch, "live deleted cm-b", ofType(gateway.EventDeleted))
	// F7: a kube-apiserver DELETED is complete and carries a uid — so the gateway never has to guess,
	// and the consumer gets an identity it can act on rather than an ambiguous tombstone.
	if deleted.Identity == nil || deleted.Identity.UID == "" {
		t.Fatalf("deleted carried no trustworthy uid: %+v", deleted.Identity)
	}
	if deleted.Identity.Name != "cm-b" {
		t.Errorf("deleted %q, want cm-b", deleted.Identity.Name)
	}
}

// §3b against an AGGREGATED API: the path that is not optional.
//
// This is F6 as an executable claim. The test first proves the API server REFUSES the streaming list
// — so that a future cluster quietly gaining WatchList cannot make this test pass for the wrong
// reason — and then proves the backend serves the scope anyway.
func TestRealClusterAggregatedAPIFallsBack(t *testing.T) {
	cs, dyn := clients(t)
	if _, err := cs.Discovery().ServerResourcesForGroupVersion("wardle.example.com/v1alpha1"); err != nil {
		t.Skip("no aggregated API installed — run `task cluster-aggregated-api`")
	}
	namespace := scratchNamespace(t, cs)
	ctx := context.Background()

	flunder := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "wardle.example.com/v1alpha1", "kind": "Flunder",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"spec":     map[string]any{"referenceType": "Flunder", "reference": "some-flunder"},
		}}
	}
	if _, err := dyn.Resource(flunders).Namespace(namespace).Create(ctx, flunder("fl-a"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create fl-a: %v", err)
	}

	// The premise, asserted rather than assumed: this API server does NOT do §3a.
	_, err := dyn.Resource(flunders).Namespace(namespace).Watch(ctx, metav1.ListOptions{
		SendInitialEvents:    ptr.To(true),
		AllowWatchBookmarks:  true,
		ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
	})
	if err == nil {
		t.Fatal("the aggregated API ACCEPTED sendInitialEvents — this cluster no longer reproduces F6, " +
			"and this test is now proving nothing. Re-run `task cluster-facts` and re-read §3b.")
	}
	t.Logf("as expected, the aggregated API refused §3a: %v", err)

	// And yet the gateway serves it: reset … added … synced, with a boundary WE synthesized.
	ch := stream(t, dyn, gateway.Scope{
		Target: "e2e", Group: "wardle.example.com", Version: "v1alpha1",
		Resource: "flunders", Namespace: namespace,
	})
	await(t, ch, "reset", ofType(gateway.EventReset))
	await(t, ch, "added fl-a", named(gateway.EventAdded, "fl-a"))
	await(t, ch, "synced (SYNTHESIZED — the API server would not give us one)", ofType(gateway.EventSynced))

	// The live tail, resumed at the list's resourceVersion, is real too.
	if _, err := dyn.Resource(flunders).Namespace(namespace).Create(ctx, flunder("fl-b"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create fl-b: %v", err)
	}
	await(t, ch, "live added fl-b", named(gateway.EventAdded, "fl-b"))

	if err := dyn.Resource(flunders).Namespace(namespace).Delete(ctx, "fl-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete fl-a: %v", err)
	}
	deleted := await(t, ch, "live deleted fl-a", ofType(gateway.EventDeleted))
	if deleted.Identity == nil || deleted.Identity.UID == "" {
		t.Fatalf("deleted carried no trustworthy uid: %+v", deleted.Identity)
	}
}
