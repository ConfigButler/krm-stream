//go:build e2e

// Real-API save composition: proposal 0006, priority 3.
//
// e2e_test.go proves the backend streams a real API server and that a stale captured save is refused
// with a real 409. These compose the whole save path instead: a real API server, the real gateway
// over the real backend, a real host save endpoint, and the REAL browser store and conditional editor,
// run by node (packages/krm-stream/e2e/store-driver.ts). Go decides when each stream event reaches the
// browser, so a race a person meets only by bad luck can be staged on purpose.
//
//	task test-real-api   these cases, against the spike cluster
//	task test-cluster    everything tagged e2e, including the aggregated API
package kube_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	"github.com/ConfigButler/krm-stream/gateway/kube/examples/conditionalsave"
)

// ---------------------------------------------------------------------------- the browser --

// storeDriver is one browser: the real LiveResourceStore and conditional editor, in a node process.
type storeDriver struct {
	t       *testing.T
	stdin   io.WriteCloser
	mu      sync.Mutex
	next    int
	waiting map[int]chan map[string]json.RawMessage
}

func newStoreDriver(t *testing.T) *storeDriver {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node runs the browser store in these tests: %v", err)
	}
	cmd := exec.Command(node, filepath.Join("..", "..", "packages", "krm-stream", "e2e", "store-driver.ts")) //nolint:gosec // a fixed path in this repository
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d := &storeDriver{t: t, stdin: stdin, waiting: map[int]chan map[string]json.RawMessage{}}
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			var reply map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &reply) != nil {
				continue
			}
			var id int
			_ = json.Unmarshal(reply["id"], &id)
			d.mu.Lock()
			ch := d.waiting[id]
			delete(d.waiting, id)
			d.mu.Unlock()
			if ch != nil {
				ch <- reply
			}
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return d
}

// start sends a command and returns where its reply will arrive, without waiting for it.
func (d *storeDriver) start(command map[string]any) <-chan map[string]json.RawMessage {
	d.mu.Lock()
	d.next++
	id := d.next
	ch := make(chan map[string]json.RawMessage, 1)
	d.waiting[id] = ch
	d.mu.Unlock()
	command["id"] = id
	line, err := json.Marshal(command)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.stdin.Write(append(line, '\n')); err != nil {
		d.t.Fatalf("browser: %v", err)
	}
	return ch
}

func (d *storeDriver) wait(ch <-chan map[string]json.RawMessage, what string) map[string]json.RawMessage {
	d.t.Helper()
	select {
	case reply := <-ch:
		if failure, ok := reply["error"]; ok {
			d.t.Fatalf("browser %s: %s", what, failure)
		}
		return reply
	case <-time.After(60 * time.Second):
		d.t.Fatalf("browser %s: no reply", what)
		return nil
	}
}

func (d *storeDriver) call(command map[string]any) map[string]json.RawMessage {
	d.t.Helper()
	return d.wait(d.start(command), command["op"].(string))
}

func (d *storeDriver) event(ev gateway.Event) {
	d.t.Helper()
	d.call(map[string]any{"op": "event", "event": ev})
}

func (d *storeDriver) set(uid string, path []any, value any) {
	d.t.Helper()
	d.call(map[string]any{"op": "set", "uid": uid, "path": path, "value": value})
}

// save runs the conditional editor's Save against the host at url, and returns its outcome.
func (d *storeDriver) save(uid, url string) string {
	d.t.Helper()
	return d.outcome(d.saveAsync(uid, url))
}

// saveAsync starts a Save and returns at once, so stream events can reach the browser meanwhile.
func (d *storeDriver) saveAsync(uid, url string) <-chan map[string]json.RawMessage {
	return d.start(map[string]any{"op": "save", "uid": uid, "url": url})
}

func (d *storeDriver) outcome(ch <-chan map[string]json.RawMessage) string {
	d.t.Helper()
	var outcome string
	if err := json.Unmarshal(d.wait(ch, "save")["outcome"], &outcome); err != nil {
		d.t.Fatal(err)
	}
	return outcome
}

type editorState struct {
	Present   bool           `json:"present"`
	Live      bool           `json:"live"`
	Draft     map[string]any `json:"draft"`
	Server    map[string]any `json:"server"`
	Changes   []any          `json:"changes"`
	Conflicts []any          `json:"conflicts"`
}

func (d *storeDriver) state(uid string) editorState {
	d.t.Helper()
	reply := d.call(map[string]any{"op": "state", "uid": uid})
	delete(reply, "id")
	raw, _ := json.Marshal(reply)
	var s editorState
	if err := json.Unmarshal(raw, &s); err != nil {
		d.t.Fatal(err)
	}
	return s
}

type hostRequest struct {
	Method string          `json:"method"`
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// requests returns the host requests the editor made since the last call.
func (d *storeDriver) requests() []hostRequest {
	d.t.Helper()
	var out []hostRequest
	if err := json.Unmarshal(d.call(map[string]any{"op": "requests"})["requests"], &out); err != nil {
		d.t.Fatal(err)
	}
	return out
}

func (d *storeDriver) expectRequests(what string, want ...string) []hostRequest {
	d.t.Helper()
	got := d.requests()
	var seen []string
	for _, r := range got {
		seen = append(seen, r.Method+" "+strconv.Itoa(r.Status))
	}
	if strings.Join(seen, ", ") != strings.Join(want, ", ") {
		d.t.Fatalf("%s: host requests = [%s], want [%s]", what, strings.Join(seen, ", "), strings.Join(want, ", "))
	}
	return got
}

// deliver forwards stream events to the browser, in order, until one satisfies pred.
func (d *storeDriver) deliver(ch <-chan gateway.Event, what string, pred func(gateway.Event) bool) gateway.Event {
	d.t.Helper()
	deadline := time.After(90 * time.Second)
	for {
		select {
		case <-deadline:
			d.t.Fatalf("timed out delivering until %s", what)
		case ev, ok := <-ch:
			if !ok {
				d.t.Fatalf("the stream closed before %s", what)
			}
			d.event(ev)
			if pred(ev) {
				return ev
			}
		}
	}
}

// ------------------------------------------------------------------------- the gateway --

// streamThrough runs the real gateway over backend with a fixed projection, and hands back its events
// and its lifecycle observations.
func streamThrough(t *testing.T, backend gateway.Backend, scope gateway.Scope, projection gateway.Projection) (<-chan gateway.Event, <-chan gateway.Observation) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	observations := make(chan gateway.Observation, 1024)
	g := &gateway.Gateway{StreamConfig: gateway.StreamConfig{
		Authorizer:  gateway.AllowAll{},
		Projections: gateway.StaticProjection(projection),
		Clients:     func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return backend, nil },
		Observer: gateway.ObserverFunc(func(o gateway.Observation) {
			select {
			case observations <- o:
			default:
			}
		}),
	}}
	sink := chanSink{ch: make(chan gateway.Event, 256)}
	go func() {
		if err := g.Stream(ctx, nil, scope, "", sink); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("stream ended: %v", err)
		}
	}()
	return sink.ch, observations
}

// closableBackend lets a test end the current upstream watch, the way an API server ends one when its
// timeout passes: the gateway then begins a new snapshot cycle on the same connection.
type closableBackend struct {
	inner gateway.Backend
	close chan struct{}
}

func (b closableBackend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	w, err := b.inner.Watch(ctx, scope)
	if err != nil {
		return nil, err
	}
	return &closableWatcher{inner: w, close: b.close}, nil
}

type closableWatcher struct {
	inner gateway.Watcher
	close <-chan struct{}
}

func (w *closableWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	next, cancel := context.WithCancel(ctx)
	defer cancel()
	closed := make(chan struct{})
	go func() {
		select {
		case <-w.close:
			close(closed)
			cancel()
		case <-next.Done():
		}
	}()
	ev, err := w.inner.Next(next)
	select {
	case <-closed:
		if ctx.Err() == nil {
			return gateway.WatchEvent{}, gateway.ErrWatchClosed
		}
	default:
	}
	return ev, err
}

func (w *closableWatcher) Stop() { w.inner.Stop() }

func awaitObservation(t *testing.T, observations <-chan gateway.Observation, what string, pred func(gateway.Observation) bool) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case o := <-observations:
			if pred(o) {
				return
			}
		}
	}
}

func suppressedModification(o gateway.Observation) bool {
	return o.Kind == gateway.ObservationEventSuppressed && o.EventType == gateway.EventModified
}

// ------------------------------------------------------------------- a status subresource --

var widgets = schema.GroupVersionResource{Group: "e2e.krm-stream.configbutler.io", Version: "v1", Resource: "widgets"}

// ensureWidgetCRD installs a namespaced Widget with a REAL status subresource, so a controller-style
// status write advances resourceVersion without touching spec. It stays installed, like the sample
// aggregated API: test objects live in scratch namespaces that are deleted.
func ensureWidgetCRD(t *testing.T, dyn dynamic.Interface) {
	t.Helper()
	ctx := t.Context()
	crds := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
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
		t.Fatalf("install the Widget CRD: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		_, err := dyn.Resource(widgets).Namespace("default").List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Widget CRD is not served: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// widgetHost is a host save endpoint for one Widget, built the way the ConfigMap example is: a
// projected GET envelope, and a merge PATCH carrying the captured UID and resourceVersion as
// preconditions. It is test-only; the ConfigMap example is not generalized for it.
type widgetHost struct {
	*httptest.Server
	mu   sync.Mutex
	hold *heldRead
}

type heldRead struct{ arrived, release chan struct{} }

// holdNextGET makes the next GET wait until release is called, and reports when it has arrived.
func (h *widgetHost) holdNextGET() (arrived <-chan struct{}, release func()) {
	hold := &heldRead{arrived: make(chan struct{}), release: make(chan struct{})}
	h.mu.Lock()
	h.hold = hold
	h.mu.Unlock()
	return hold.arrived, func() { close(hold.release) }
}

func newWidgetHost(t *testing.T, dyn dynamic.Interface, namespace, name string, projection gateway.Projection) *widgetHost {
	t.Helper()
	h := &widgetHost{}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			h.mu.Lock()
			hold := h.hold
			h.hold = nil
			h.mu.Unlock()
			if hold != nil {
				close(hold.arrived)
				<-hold.release
			}
		}
		client := dyn.Resource(widgets).Namespace(namespace)
		current, err := client.Get(r.Context(), name, metav1.GetOptions{})
		if err != nil {
			http.Error(w, err.Error(), statusCode(err))
			return
		}
		switch r.Method {
		case http.MethodGet:
			projected, paths := gateway.Project(projection, current.Object)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": projected, "redactedPaths": paths})
		case http.MethodPatch:
			var request struct {
				UID             string         `json:"uid"`
				ResourceVersion string         `json:"resourceVersion"`
				Patch           map[string]any `json:"patch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.UID == "" || request.ResourceVersion == "" {
				http.Error(w, "invalid save request", http.StatusBadRequest)
				return
			}
			if request.UID != string(current.GetUID()) {
				http.Error(w, "object was replaced", http.StatusConflict)
				return
			}
			for key := range request.Patch {
				if key != "spec" {
					http.Error(w, "field is not editable", http.StatusBadRequest)
					return
				}
			}
			request.Patch["metadata"] = map[string]any{"uid": request.UID, "resourceVersion": request.ResourceVersion}
			patch, _ := json.Marshal(request.Patch)
			if err := gateway.ValidateMergePatch(projection, current.Object, patch); err != nil {
				http.Error(w, "patch touches a protected field", http.StatusBadRequest)
				return
			}
			if _, err := client.Patch(r.Context(), name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
				http.Error(w, err.Error(), statusCode(err))
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func statusCode(err error) int {
	var apiStatus apierrors.APIStatus
	if errors.As(err, &apiStatus) {
		return int(apiStatus.Status().Code)
	}
	return http.StatusBadGateway
}

// widgetRig is one Widget, its live stream, a browser holding it and a host to save it through.
type widgetRig struct {
	t            *testing.T
	dyn          dynamic.Interface
	ns, uid      string
	events       <-chan gateway.Event
	observations <-chan gateway.Observation
	closeWatch   chan struct{}
	browser      *storeDriver
	host         *widgetHost
}

func newWidgetRig(t *testing.T, projection gateway.Projection) *widgetRig {
	t.Helper()
	cs, dyn := clients(t)
	ensureWidgetCRD(t, dyn)
	ns := scratchNamespace(t, cs)
	created, err := dyn.Resource(widgets).Namespace(ns).Create(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgets.GroupVersion().String(),
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "w"},
		"spec":       map[string]any{"image": "v1", "replicas": int64(1)},
	}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create widget: %v", err)
	}
	closeWatch := make(chan struct{}, 1)
	backend := closableBackend{inner: kube.NewBackend(dyn), close: closeWatch}
	scope := gateway.Scope{Group: widgets.Group, Version: widgets.Version, Resource: widgets.Resource, Namespace: ns, Name: "w"}
	events, observations := streamThrough(t, backend, scope, projection)
	r := &widgetRig{
		t: t, dyn: dyn, ns: ns, uid: string(created.GetUID()),
		events: events, observations: observations, closeWatch: closeWatch,
		browser: newStoreDriver(t), host: newWidgetHost(t, dyn, ns, "w", projection),
	}
	r.browser.deliver(events, "the initial snapshot", ofType(gateway.EventSynced))
	return r
}

func (r *widgetRig) current() *unstructured.Unstructured {
	r.t.Helper()
	got, err := r.dyn.Resource(widgets).Namespace(r.ns).Get(r.t.Context(), "w", metav1.GetOptions{})
	if err != nil {
		r.t.Fatal(err)
	}
	return got
}

// writeStatus is a controller's status write, through the status subresource. It returns the
// resourceVersion before and after, having checked that the write really advanced it.
func (r *widgetRig) writeStatus(status map[string]any) (before, after string) {
	r.t.Helper()
	obj := r.current()
	before = obj.GetResourceVersion()
	obj.Object["status"] = status
	updated, err := r.dyn.Resource(widgets).Namespace(r.ns).UpdateStatus(r.t.Context(), obj, metav1.UpdateOptions{})
	if err != nil {
		r.t.Fatalf("status write: %v", err)
	}
	after = updated.GetResourceVersion()
	requireNewer(r.t, "a persisted status write", before, after)
	return before, after
}

// requireNewer fails unless after is a later resourceVersion than before. Kubernetes 1.35+ makes
// them orderable decimals; a write that does not advance one changed nothing and proves nothing.
func requireNewer(t *testing.T, what, before, after string) {
	t.Helper()
	b, errB := strconv.ParseUint(before, 10, 64)
	a, errA := strconv.ParseUint(after, 10, 64)
	if errB != nil || errA != nil || a <= b {
		t.Fatalf("%s did not advance resourceVersion: %s -> %s", what, before, after)
	}
}

// requireQuiet fails if the stream has delivered anything the browser has not seen.
func (r *widgetRig) requireQuiet(what string) {
	r.t.Helper()
	select {
	case ev := <-r.events:
		r.t.Fatalf("%s reached the stream as %s", what, ev.Type)
	default:
	}
}

func resourceVersionOf(object map[string]any) string {
	meta, _ := object["metadata"].(map[string]any)
	rv, _ := meta["resourceVersion"].(string)
	return rv
}

func specOf(object map[string]any) map[string]any {
	spec, _ := object["spec"].(map[string]any)
	return spec
}

// ------------------------------------------------------------------------------- the cases --

// Status churn under krm-spec/v1: the write advances the version but not the stream, so a save captured
// against the delivered version is refused, and the guarded projected read advances the base with no
// field conflicts. The person's edit survives, and a fresh intent saves once the churn stops.
func TestRealAPIStatusChurnUnderSpecProjection(t *testing.T) {
	r := newWidgetRig(t, gateway.ProjectionSpec)
	r.browser.set(r.uid, []any{"spec", "image"}, "v2")

	_, after := r.writeStatus(map[string]any{"ready": true})
	awaitObservation(t, r.observations, "the status-only update to be suppressed", suppressedModification)
	r.requireQuiet("a status-only update under krm-spec/v1")

	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "version-stale" {
		t.Fatalf("save after unseen status churn = %q, want version-stale", outcome)
	}
	r.browser.expectRequests("stale save", "PATCH 409", "GET 200")
	s := r.browser.state(r.uid)
	if len(s.Conflicts) != 0 || resourceVersionOf(s.Server) != after || specOf(s.Draft)["image"] != "v2" {
		t.Fatalf("after the guarded read: conflicts=%v base=%s draft=%v", s.Conflicts, resourceVersionOf(s.Server), specOf(s.Draft))
	}
	if _, present := s.Server["status"]; present {
		t.Fatal("the guarded read leaked status past krm-spec/v1")
	}
	if image := specOf(r.current().Object)["image"]; image != "v1" {
		t.Fatalf("the refused save wrote anyway: image=%v", image)
	}

	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "saved" {
		t.Fatalf("fresh save = %q, want saved", outcome)
	}
	r.browser.expectRequests("fresh save", "PATCH 204")
	stored := r.current().Object
	ready, _ := stored["status"].(map[string]any)
	if specOf(stored)["image"] != "v2" || ready["ready"] != true {
		t.Fatalf("stored object = spec %v status %v; want the edit and the controller's status", specOf(stored), stored["status"])
	}
	r.browser.deliver(r.events, "the echo of the save", ofType(gateway.EventModified))
	if s := r.browser.state(r.uid); len(s.Changes) != 0 {
		t.Fatalf("the echo did not settle the draft: %v", s.Changes)
	}
}

// The contrast: under krm-full/v1 the same status write is visible, so it reaches the browser as an
// ordinary update, the base advances with it, and the next save needs no refusal at all.
func TestRealAPIStatusChangeIsDeliveredUnderFullProjection(t *testing.T) {
	r := newWidgetRig(t, gateway.ProjectionFull)
	r.browser.set(r.uid, []any{"spec", "image"}, "v2")
	_, after := r.writeStatus(map[string]any{"ready": true})
	update := r.browser.deliver(r.events, "the status update", ofType(gateway.EventModified))
	if resourceVersionOf(update.Object) != after {
		t.Fatalf("delivered version %s, want %s", resourceVersionOf(update.Object), after)
	}
	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "saved" {
		t.Fatalf("save = %q, want saved", outcome)
	}
	r.browser.expectRequests("save after a delivered update", "PATCH 204")
}

// Bookkeeping-only under krm-full/v1: a new server-side-apply manager claiming a value the object
// already has changes only managedFields. That still advances the version, so it is a valid fixture
// (a no-op reapply would not be), and full projection suppresses it exactly as spec projection
// suppresses status.
func TestRealAPIBookkeepingOnlyWriteUnderFullProjection(t *testing.T) {
	r := newWidgetRig(t, gateway.ProjectionFull)
	r.browser.set(r.uid, []any{"spec", "image"}, "v2")

	before := r.current().GetResourceVersion()
	applied, err := r.dyn.Resource(widgets).Namespace(r.ns).Apply(t.Context(), "w", &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": widgets.GroupVersion().String(),
		"kind":       "Widget",
		"metadata":   map[string]any{"name": "w", "namespace": r.ns},
		"spec":       map[string]any{"image": "v1"},
	}}, metav1.ApplyOptions{FieldManager: "e2e-bookkeeper"})
	if err != nil {
		t.Fatalf("server-side apply: %v", err)
	}
	requireNewer(t, "a bookkeeping-only write", before, applied.GetResourceVersion())
	awaitObservation(t, r.observations, "the bookkeeping-only update to be suppressed", suppressedModification)
	r.requireQuiet("a bookkeeping-only update under krm-full/v1")

	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "version-stale" {
		t.Fatalf("save = %q, want version-stale", outcome)
	}
	r.browser.expectRequests("stale save", "PATCH 409", "GET 200")
	if s := r.browser.state(r.uid); len(s.Conflicts) != 0 || resourceVersionOf(s.Server) != applied.GetResourceVersion() {
		t.Fatalf("after the guarded read: conflicts=%v base=%s", s.Conflicts, resourceVersionOf(s.Server))
	}
	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "saved" {
		t.Fatalf("fresh save = %q, want saved", outcome)
	}
	r.browser.expectRequests("fresh save", "PATCH 204")
}

// Overlapping recovery: the save starts live and is refused because status moved the version; while
// its reconciliation read is in flight the upstream watch ends and a new snapshot begins; the read
// lands before synced. The guard refuses it, the draft stays, the editor reports recovery, and progress
// resumes after the snapshot with a read and then one deliberate write: never a blind PATCH retry.
func TestRealAPIOverlappingRecovery(t *testing.T) {
	r := newWidgetRig(t, gateway.ProjectionSpec)
	r.browser.set(r.uid, []any{"spec", "image"}, "v2")
	before, _ := r.writeStatus(map[string]any{"ready": true})
	awaitObservation(t, r.observations, "the status-only update to be suppressed", suppressedModification)

	arrived, release := r.host.holdNextGET()
	saving := r.browser.saveAsync(r.uid, r.host.URL)
	select {
	case <-arrived: // refused with 409; the editor is now reading
	case <-time.After(60 * time.Second):
		t.Fatal("the editor never made its reconciliation read")
	}
	r.closeWatch <- struct{}{} // the API server ends the watch
	r.browser.deliver(r.events, "the recovery snapshot's reset", ofType(gateway.EventReset))
	release()
	if outcome := r.browser.outcome(saving); outcome != "recovering" {
		t.Fatalf("save overlapping a snapshot = %q, want recovering", outcome)
	}
	r.browser.expectRequests("the overlapping save", "PATCH 409", "GET 200")
	// Refused, not merely presented as recovery: the base is still the version the stream delivered.
	if s := r.browser.state(r.uid); s.Live || resourceVersionOf(s.Server) != before || specOf(s.Draft)["image"] != "v2" {
		t.Fatalf("during recovery: live=%v base=%s draft=%v; the read must be refused and the draft kept",
			s.Live, resourceVersionOf(s.Server), specOf(s.Draft))
	}

	r.browser.deliver(r.events, "the recovery snapshot", ofType(gateway.EventSynced))
	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "version-stale" {
		t.Fatalf("first save after recovery = %q, want version-stale", outcome)
	}
	r.browser.expectRequests("first save after recovery: a read, no write", "GET 200")
	if outcome := r.browser.save(r.uid, r.host.URL); outcome != "saved" {
		t.Fatalf("deliberate save = %q, want saved", outcome)
	}
	r.browser.expectRequests("deliberate save", "PATCH 204")
	if image := specOf(r.current().Object)["image"]; image != "v2" {
		t.Fatalf("stored image = %v", image)
	}
}

// barrierTransport runs fire before each request reaches the API server: a test-only hook between
// the host's preflight GET and its PATCH.
type barrierTransport struct {
	next http.RoundTripper
	fire func(*http.Request)
}

func (b barrierTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	b.fire(r)
	return b.next.RoundTrip(r)
}

// The UID race: the ConfigMap is deleted and recreated after the host's preflight GET and before its
// PATCH. Kubernetes refuses the stale resourceVersion first, so the host classifies the failure with
// one GET and names the replacement in a structured Status. The replacement is untouched, the editor
// reports the object unavailable, and the replacement opens without the old draft.
func TestRealAPIReplacementBetweenPreflightAndPatch(t *testing.T) {
	cs, dyn := clients(t)
	ns := scratchNamespace(t, cs)
	ctx := t.Context()
	original, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "editor"}, Data: map[string]string{"value": "base"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	events := stream(t, dyn, gateway.Scope{Version: "v1", Resource: "configmaps", Namespace: ns, Name: "editor"})
	browser := newStoreDriver(t)
	browser.deliver(events, "the initial snapshot", ofType(gateway.EventSynced))
	uid := string(original.UID)
	browser.set(uid, []any{"data", "value"}, "mine")

	var once sync.Once
	var replacement *corev1.ConfigMap
	config := restConfig(t)
	config.WrapTransport = func(next http.RoundTripper) http.RoundTripper {
		return barrierTransport{next: next, fire: func(r *http.Request) {
			if r.Method != http.MethodPatch {
				return
			}
			once.Do(func() {
				if err := cs.CoreV1().ConfigMaps(ns).Delete(ctx, "editor", metav1.DeleteOptions{}); err != nil {
					t.Errorf("barrier delete: %v", err)
				}
				replacement, err = cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "editor"}, Data: map[string]string{"value": "replacement"}}, metav1.CreateOptions{})
				if err != nil {
					t.Errorf("barrier recreate: %v", err)
				}
			})
		}}
	}
	raced, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(conditionalsave.Handler(func(*http.Request) (kubernetes.Interface, error) { return raced, nil }, ns, "editor"))
	t.Cleanup(host.Close)

	if outcome := browser.save(uid, host.URL); outcome != "unavailable" {
		t.Fatalf("save against a replaced object = %q, want unavailable", outcome)
	}
	if replacement == nil {
		t.Fatal("the barrier never ran: the race was not staged")
	}
	requests := browser.expectRequests("save against a replacement", "PATCH 409", "GET 200")
	var status metav1.Status
	if err := json.Unmarshal(requests[0].Body, &status); err != nil || status.Kind != "Status" || status.Code != http.StatusConflict {
		t.Fatalf("the refusal is not a structured Kubernetes Status: %s", requests[0].Body)
	}
	if status.Message != "object was replaced" || status.Details == nil || status.Details.UID != replacement.UID {
		t.Fatalf("the identity mismatch was not named: %+v", status)
	}

	stored, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, "editor", metav1.GetOptions{})
	if err != nil || stored.UID != replacement.UID || stored.Data["value"] != "replacement" {
		t.Fatalf("the replacement changed: %+v (%v)", stored, err)
	}
	browser.deliver(events, "the replacement's arrival", func(ev gateway.Event) bool {
		return ev.Type == gateway.EventAdded && ev.Object.UID() == string(replacement.UID)
	})
	if s := browser.state(uid); s.Present {
		t.Fatal("the deleted original is still in the store")
	}
	s := browser.state(string(replacement.UID))
	data, _ := s.Draft["data"].(map[string]any)
	if !s.Present || len(s.Changes) != 0 || data["value"] != "replacement" {
		t.Fatalf("the replacement inherited the old draft: %+v", s)
	}
}

// A real validation error keeps its code, reason and field causes through the host. It is the
// person's input to fix, never a conflict to reload.
func TestRealAPIValidationErrorIsNotAConflict(t *testing.T) {
	cs, _ := clients(t)
	ns := scratchNamespace(t, cs)
	ctx := t.Context()
	cm, err := cs.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "editor"}, Data: map[string]string{"value": "base"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	handler := conditionalsave.Handler(func(*http.Request) (kubernetes.Interface, error) { return cs, nil }, ns, cm.Name)
	body, _ := json.Marshal(map[string]any{
		"uid": string(cm.UID), "resourceVersion": cm.ResourceVersion,
		"patch": map[string]any{"metadata": map[string]any{"labels": map[string]any{"team": "not a valid label value!"}}},
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPatch, "/editor", strings.NewReader(string(body))))
	var status metav1.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("not a Status: %s", response.Body.String())
	}
	if response.Code != http.StatusUnprocessableEntity || status.Reason != metav1.StatusReasonInvalid {
		t.Fatalf("HTTP %d %+v; a validation error is not a conflict", response.Code, status)
	}
	if status.Details == nil || len(status.Details.Causes) == 0 || !strings.HasPrefix(status.Details.Causes[0].Field, "metadata.labels") {
		t.Fatalf("the field causes were lost: %+v", status.Details)
	}
	stored, err := cs.CoreV1().ConfigMaps(ns).Get(ctx, cm.Name, metav1.GetOptions{})
	if err != nil || stored.ResourceVersion != cm.ResourceVersion {
		t.Fatalf("the invalid save changed the object: %v", err)
	}
}
