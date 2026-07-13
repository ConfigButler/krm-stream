package kube

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"

	"github.com/ConfigButler/krm-stream/gateway"
)

// The upstream is stubbed rather than faked with client-go's fake dynamic client, and the reason is
// specific: the fake's watch reactor cannot see `SendInitialEvents` at all (a WatchAction carries
// only label/field/resourceVersion restrictions). The exact ListOptions we send ARE the thing under
// test here — they are what a real API server accepted (F1) and what a real aggregated API refused
// (F6) — so a test that could not see them would be testing nothing.

type stubResource struct {
	// Embedded nil interface: any method this test does not override panics rather than silently
	// returning a zero value, which is what we want from a stub.
	dynamic.ResourceInterface

	t *testing.T

	watchFn func(opts metav1.ListOptions) (watch.Interface, error)
	listFn  func(opts metav1.ListOptions) (*unstructured.UnstructuredList, error)

	watchOpts []metav1.ListOptions // every Watch this stub was asked for, in order
	listOpts  []metav1.ListOptions
}

func (r *stubResource) Watch(_ context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	r.watchOpts = append(r.watchOpts, opts)
	return r.watchFn(opts)
}

func (r *stubResource) List(_ context.Context, opts metav1.ListOptions) (*unstructured.UnstructuredList, error) {
	r.listOpts = append(r.listOpts, opts)
	if r.listFn == nil {
		r.t.Fatal("List was called, and this test did not expect it")
	}
	return r.listFn(opts)
}

type stubNamespaceable struct {
	*stubResource
	namespaces []string
}

func (n *stubNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	n.namespaces = append(n.namespaces, ns)
	return n.stubResource
}

type stubClient struct {
	ns   *stubNamespaceable
	gvrs []schema.GroupVersionResource
}

func (c *stubClient) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	c.gvrs = append(c.gvrs, gvr)
	return c.ns
}

func newStub(t *testing.T) (*stubClient, *stubResource) {
	t.Helper()
	res := &stubResource{t: t}
	return &stubClient{ns: &stubNamespaceable{stubResource: res}}, res
}

// refusal is the error a REAL aggregated API server returned when handed the §3a request
// (docs/facts/observed-v1.36.2+k3s1.md, F6). Reproducing its exact shape is the point: the fallback
// hangs off recognising it, and a hand-waved "some error" would prove nothing.
func refusal() error {
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "meta.k8s.io", Kind: "ListOptions"}, "",
		field.ErrorList{field.Forbidden(
			field.NewPath("sendInitialEvents"),
			"sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled",
		)},
	)
}

func obj(name, uid, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name": name, "namespace": "ns", "uid": uid, "resourceVersion": rv,
		},
		"data": map[string]any{"k": "v"},
	}}
}

func bookmark(rv string, initialEventsEnd bool) *unstructured.Unstructured {
	// What a REAL bookmark looks like (F1): a resourceVersion, the annotation carrying the marker —
	// and NO uid, which is what the gateway's partial-object guard keys on.
	meta := map[string]any{"resourceVersion": rv}
	if initialEventsEnd {
		meta["annotations"] = map[string]any{initialEventsEndAnnotation: "true"}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta,
	}}
}

var scope = gateway.Scope{
	Target: "prod", Version: "v1", Resource: "configmaps", Namespace: "ns",
}

func drain(t *testing.T, w gateway.Watcher, n int) []gateway.WatchEvent {
	t.Helper()
	ctx := t.Context()
	var got []gateway.WatchEvent
	for range n {
		ev, err := w.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v (after %d events)", err, len(got))
		}
		got = append(got, ev)
	}
	return got
}

// §3a, the primary path: the exact request a real v1.36.2 API server accepted.
func TestStreamingListSendsTheOptionsTheClusterVerified(t *testing.T) {
	client, res := newStub(t)
	fake := watch.NewFakeWithChanSize(3, false)
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return fake, nil }

	w, err := NewBackend(client).Watch(t.Context(), scope)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	opts := res.watchOpts[0]
	if opts.SendInitialEvents == nil || !*opts.SendInitialEvents {
		t.Error("SendInitialEvents must be true: it is what makes the watch a snapshot")
	}
	if !opts.AllowWatchBookmarks {
		t.Error("AllowWatchBookmarks must be true: the bookmark IS the snapshot boundary")
	}
	if opts.ResourceVersionMatch != metav1.ResourceVersionMatchNotOlderThan {
		t.Errorf("ResourceVersionMatch = %q, want NotOlderThan", opts.ResourceVersionMatch)
	}
	if opts.ResourceVersion != "" {
		t.Errorf("ResourceVersion = %q, want \"\" (a consistent read of the freshest state)", opts.ResourceVersion)
	}
	if got := client.gvrs[0]; got != (schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}) {
		t.Errorf("GVR = %v", got)
	}
	if got := client.ns.namespaces; len(got) != 1 || got[0] != "ns" {
		t.Errorf("namespaces = %v, want [ns]", got)
	}

	// The snapshot, then its boundary, then live traffic — and the boundary is the API server's.
	fake.Add(obj("a", "uid-a", "10"))
	fake.Action(watch.Bookmark, bookmark("11", true))
	fake.Modify(obj("a", "uid-a", "12"))

	got := drain(t, w, 3)
	if got[0].Type != gateway.WatchAdded || got[0].Object.UID() != "uid-a" {
		t.Errorf("event 0 = %+v, want added uid-a", got[0])
	}
	if got[1].Type != gateway.WatchBookmark || !got[1].InitialEventsEnd {
		t.Errorf("event 1 = %+v, want the initial-events-end bookmark", got[1])
	}
	if got[2].Type != gateway.WatchModified {
		t.Errorf("event 2 = %+v, want modified", got[2])
	}
}

// A routine bookmark is NOT the boundary. Mistaking one for the other fires `synced` early, and the
// consumer prunes objects it has not been sent yet.
func TestRoutineBookmarkIsNotTheBoundary(t *testing.T) {
	client, res := newStub(t)
	fake := watch.NewFakeWithChanSize(1, false)
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return fake, nil }

	w, err := NewBackend(client).Watch(t.Context(), scope)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	fake.Action(watch.Bookmark, bookmark("11", false))
	got := drain(t, w, 1)[0]
	if got.Type != gateway.WatchBookmark {
		t.Fatalf("type = %v, want bookmark", got.Type)
	}
	if got.InitialEventsEnd {
		t.Error("a bookmark WITHOUT the annotation was treated as the snapshot boundary")
	}
}

// F6, and the bug this rung exists to have caught: an aggregated API refuses §3a outright. A gateway
// that implemented only the streaming list could not open a stream for a Flunder AT ALL.
func TestAggregatedAPIRefusalFallsBackToListThenWatch(t *testing.T) {
	client, res := newStub(t)
	fake := watch.NewFakeWithChanSize(1, false)

	res.watchFn = func(opts metav1.ListOptions) (watch.Interface, error) {
		if opts.SendInitialEvents != nil {
			return nil, refusal()
		}
		return fake, nil
	}
	res.listFn = func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		list := &unstructured.UnstructuredList{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMapList",
			"metadata": map[string]any{"resourceVersion": "42"},
		}}
		list.Items = []unstructured.Unstructured{*obj("a", "uid-a", "40"), *obj("b", "uid-b", "41")}
		return list, nil
	}

	w, err := NewBackend(client).Watch(t.Context(), scope)
	if err != nil {
		t.Fatalf("the fallback did not happen: %v", err)
	}
	defer w.Stop()

	// The live watch must resume at EXACTLY the list's resourceVersion — that is what closes the gap
	// between the two calls, and it is the whole reason §3b is correct rather than merely plausible.
	live := res.watchOpts[1]
	if live.ResourceVersion != "42" {
		t.Errorf("the live watch opened at resourceVersion %q, want \"42\" (the list's) — that is a GAP", live.ResourceVersion)
	}
	if live.SendInitialEvents != nil {
		t.Error("the fallback watch must not re-send sendInitialEvents; the server just refused it")
	}
	if !live.AllowWatchBookmarks {
		t.Error("AllowWatchBookmarks should stay on: routine bookmarks still carry a resourceVersion")
	}

	fake.Modify(obj("a", "uid-a", "43"))

	// The stream loop must not be able to tell this from §3a: added, added, boundary, then live.
	got := drain(t, w, 4)
	if got[0].Type != gateway.WatchAdded || got[0].Object.UID() != "uid-a" {
		t.Errorf("event 0 = %+v, want added uid-a", got[0])
	}
	if got[1].Type != gateway.WatchAdded || got[1].Object.UID() != "uid-b" {
		t.Errorf("event 1 = %+v, want added uid-b", got[1])
	}
	if got[2].Type != gateway.WatchBookmark || !got[2].InitialEventsEnd {
		t.Errorf("event 2 = %+v, want a SYNTHESIZED initial-events-end bookmark", got[2])
	}
	if got[3].Type != gateway.WatchModified || got[3].Object.UID() != "uid-a" {
		t.Errorf("event 3 = %+v, want the live modified", got[3])
	}
}

// The refusal is a fact about the SERVER, not about the request. Re-asking every cycle would spend a
// doomed round-trip per cycle, forever, on exactly the APIs that are slowest to reach.
func TestTheRefusalIsRememberedPerGroupVersion(t *testing.T) {
	client, res := newStub(t)
	streamingAttempts := 0
	res.watchFn = func(opts metav1.ListOptions) (watch.Interface, error) {
		if opts.SendInitialEvents != nil {
			streamingAttempts++
			return nil, refusal()
		}
		return watch.NewFakeWithChanSize(1, false), nil
	}
	res.listFn = func(metav1.ListOptions) (*unstructured.UnstructuredList, error) {
		return &unstructured.UnstructuredList{Object: map[string]any{
			"metadata": map[string]any{"resourceVersion": "1"},
		}}, nil
	}

	b := NewBackend(client)
	for i := range 3 {
		w, err := b.Watch(t.Context(), scope)
		if err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		w.Stop()
	}

	if streamingAttempts != 1 {
		t.Errorf("the streaming list was attempted %d times across 3 cycles, want 1: the refusal is not being remembered", streamingAttempts)
	}

	// …and a DIFFERENT API is not tarred with the same brush: WatchList being off in one aggregated
	// server says nothing about kube-apiserver, which is the mistake in the other direction.
	other := scope
	other.Group, other.Version, other.Resource = "wardle.example.com", "v1alpha1", "flunders"
	if _, err := b.Watch(t.Context(), other); err != nil {
		t.Fatalf("other GroupVersion: %v", err)
	}
	if streamingAttempts != 2 {
		t.Errorf("streaming attempts = %d, want 2: a second GroupVersion must be tried on its own merits", streamingAttempts)
	}
}

// The fallback must NOT be a catch-all. "You may not watch Secrets" is not "your server cannot
// stream lists", and a gateway that retried the second as the first would report the wrong reason —
// or worse, succeed at listing something the caller was just denied a watch on.
func TestARealErrorIsNotSwallowedByTheFallback(t *testing.T) {
	client, res := newStub(t)
	denied := apierrors.NewForbidden(
		schema.GroupResource{Resource: "secrets"}, "", errors.New("user cannot watch secrets"))
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return nil, denied }
	// res.listFn stays nil: if the fallback fires, List t.Fatal's, which is the assertion.

	_, err := NewBackend(client).Watch(t.Context(), scope)
	if err == nil {
		t.Fatal("a Forbidden was swallowed; the caller was told the stream opened")
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("the original error was lost: %v", err)
	}
}

// A clean close is not an error — an API server times a watch out routinely. It means "reopen", with
// a fresh snapshot cycle, because we can no longer promise we saw everything in between.
func TestAClosedWatchIsErrWatchClosed(t *testing.T) {
	client, res := newStub(t)
	fake := watch.NewFakeWithChanSize(1, false)
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return fake, nil }

	w, err := NewBackend(client).Watch(t.Context(), scope)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	fake.Stop() // the API server hung up

	if _, err := w.Next(t.Context()); !errors.Is(err, gateway.ErrWatchClosed) {
		t.Errorf("Next after close = %v, want ErrWatchClosed", err)
	}
}

// F3: how a 410 Gone actually reaches an OPEN watch — as an event carrying a *metav1.Status, because
// there is no response left to put a status code on.
func TestExpiredResourceVersionArrivesAsARecoverableWatchError(t *testing.T) {
	client, res := newStub(t)
	fake := watch.NewFakeWithChanSize(1, false)
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) { return fake, nil }

	w, err := NewBackend(client).Watch(t.Context(), scope)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	fake.Error(&metav1.Status{
		Status: metav1.StatusFailure, Code: 410, Reason: metav1.StatusReasonExpired,
		Message: "too old resource version: 1 (12345)",
	})

	got := drain(t, w, 1)[0]
	if got.Type != gateway.WatchError {
		t.Fatalf("type = %v, want error", got.Type)
	}
	var se *gateway.StreamError
	if !errors.As(got.Err, &se) {
		t.Fatalf("Err = %v (%T), want a *gateway.StreamError", got.Err, got.Err)
	}
	if se.Code != gateway.CodeResyncRequired {
		t.Errorf("code = %v, want RESYNC_REQUIRED", se.Code)
	}
	if se.Terminal {
		t.Error("a 410 is NOT terminal: a new snapshot cycle follows on the same connection")
	}
}

// A named scope rides the same watch as everything else — as a field selector, not a Get. One code
// path, one stream, whether the scope is one object or a thousand.
func TestNamedScopeIsAFieldSelectorAndClusterScopeSkipsNamespace(t *testing.T) {
	client, res := newStub(t)
	res.watchFn = func(metav1.ListOptions) (watch.Interface, error) {
		return watch.NewFakeWithChanSize(1, false), nil
	}

	named := gateway.Scope{
		Target: "prod", Version: "v1", Resource: "configmaps",
		Name: "app-config", LabelSelector: "tier=web",
	}
	if _, err := NewBackend(client).Watch(t.Context(), named); err != nil {
		t.Fatalf("Watch: %v", err)
	}

	opts := res.watchOpts[0]
	if opts.FieldSelector != "metadata.name=app-config" {
		t.Errorf("FieldSelector = %q, want metadata.name=app-config", opts.FieldSelector)
	}
	if opts.LabelSelector != "tier=web" {
		t.Errorf("LabelSelector = %q, want tier=web", opts.LabelSelector)
	}
	// No namespace on the scope => cluster-scoped, and Namespace() must not be called at all.
	if got := client.ns.namespaces; len(got) != 0 {
		t.Errorf("Namespace() was called with %v for a cluster-scoped watch", got)
	}
}
