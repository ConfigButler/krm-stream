//go:build e2e

// Native viewing and editing against a real API server: the browser's connectNativeWatch and the
// native editor example, run by node (packages/krm-stream/e2e/native-driver.ts), through a host-style
// /k8s proxy that holds the credentials. No gateway is involved; this checks that a real LIST and
// WATCH — typed collections whose items carry no apiVersion or kind, selector exits, deletes and
// same-name recreation — reach the store as the connector promises, and that a conditional native
// write through the same proxy saves, recovers from a stale version and never lands on a replacement.
//
//	task test-real-api
package kube_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// nativeHost is the host: it serves the API server's paths under /k8s with its own credentials. The
// browser never holds a Kubernetes credential. It forwards GET, and — once writable — a PATCH that is
// a conditional JSON merge patch gateway.ValidateNativeMergePatch accepts.
type nativeHost struct {
	*httptest.Server
	mu sync.Mutex
	// beforePatch runs before the next accepted PATCH is forwarded, once: the place a test lands a
	// competing write between the browser capturing its save and Kubernetes checking it.
	beforePatch func()
	patches     []int // the API server's answer to each forwarded PATCH

	// Test-only watch controls for the resume case. They change nothing until a test uses them.
	gets    []forwardedGet             // every forwarded GET, in order
	watches map[int]context.CancelFunc // the open WATCH connections, by index into gets
	hold    chan struct{}              // non-nil: the next WATCH waits until it is closed
	held    chan struct{}              // closed once the next WATCH is being held
	expire  int                        // how many of the next WATCHes are rewritten to resourceVersion=1
}

// forwardedGet is one GET the host forwarded: a LIST, a WATCH or the editor's object read.
type forwardedGet struct {
	Path  string
	Watch bool
	// Asked is the resourceVersion the browser asked a WATCH to start from; Sent is what the host
	// forwarded, which differs only when the test induced an expiry.
	Asked, Sent string
	// Answer is the API server's HTTP status. A WATCH whose history expired is still answered 200:
	// the 410 arrives as an ERROR event in the stream.
	Answer int
}

type forwardedKey struct{}

// lists counts the forwarded LISTs and WATCHes of one collection path.
func (h *nativeHost) lists(path string) (lists, watches int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, g := range h.gets {
		switch {
		case g.Path != path:
		case g.Watch:
			watches++
		default:
			lists++
		}
	}
	return lists, watches
}

// forwardedWatches returns every forwarded WATCH, in order.
func (h *nativeHost) forwardedWatches() []forwardedGet {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.DeleteFunc(slices.Clone(h.gets), func(g forwardedGet) bool { return !g.Watch })
}

// dropWatches cuts every open WATCH connection, as a network failure would, and says how many.
func (h *nativeHost) dropWatches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, cancel := range h.watches {
		cancel()
	}
	return len(h.watches)
}

// holdNextWatch makes the next WATCH wait, so the test can change the cluster while the browser is
// disconnected. held is closed once that WATCH arrives; release lets it through.
func (h *nativeHost) holdNextWatch() (held <-chan struct{}, release func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hold := make(chan struct{})
	h.hold, h.held = hold, make(chan struct{})
	return h.held, sync.OnceFunc(func() { close(hold) })
}

// expireNextWatches rewrites the resourceVersion of the next n WATCHes to "1", long compacted away,
// so the REAL API server answers each with its own in-stream 410 Expired. The expiry is induced; the
// 410 is the server's.
func (h *nativeHost) expireNextWatches(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expire = n
}

// forwardGet applies the watch controls to one GET and records it. It returns the request to forward
// and a function to call once the response is over.
func (h *nativeHost) forwardGet(r *http.Request) (*http.Request, func()) {
	query := r.URL.Query()
	watch := query.Get("watch") == "1" || query.Get("watch") == "true"
	if watch {
		h.mu.Lock()
		hold, held := h.hold, h.held
		h.hold, h.held = nil, nil
		h.mu.Unlock()
		if hold != nil {
			close(held)
			select {
			case <-hold:
			case <-r.Context().Done():
			}
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	get := forwardedGet{Path: r.URL.Path, Watch: watch}
	if watch {
		get.Asked = query.Get("resourceVersion")
		get.Sent = get.Asked
		if h.expire > 0 {
			h.expire--
			get.Sent = "1"
			query.Set("resourceVersion", get.Sent)
			r.URL.RawQuery = query.Encode()
		}
	}
	index := len(h.gets)
	h.gets = append(h.gets, get)
	ctx, cancel := context.WithCancel(context.WithValue(r.Context(), forwardedKey{}, index))
	if watch {
		h.watches[index] = cancel
	}
	return r.WithContext(ctx), func() {
		cancel()
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.watches, index)
	}
}

func (h *nativeHost) nextPatch(hook func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.beforePatch = hook
}

func (h *nativeHost) answered() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.patches)
}

func nativeProxy(t *testing.T, writable bool) *nativeHost {
	t.Helper()
	cfg := restConfig(t)
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	target, err := url.Parse(cfg.Host)
	if err != nil {
		t.Fatalf("API server address: %v", err)
	}
	host := &nativeHost{watches: map[int]context.CancelFunc{}}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = target.Scheme
			r.Out.URL.Host = target.Host
			r.Out.Host = target.Host
			r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, "/k8s")
			r.Out.URL.RawPath = ""
		},
		Transport: transport,
		// A watch is a long response; every frame must reach the browser as it is written.
		FlushInterval: -1,
		ModifyResponse: func(res *http.Response) error {
			host.mu.Lock()
			defer host.mu.Unlock()
			if res.Request.Method == http.MethodPatch {
				host.patches = append(host.patches, res.StatusCode)
			}
			if index, ok := res.Request.Context().Value(forwardedKey{}).(int); ok {
				host.gets[index].Answer = res.StatusCode
			}
			return nil
		},
	}
	host.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/k8s/") {
			http.Error(w, "not proxied", http.StatusNotFound)
			return
		}
		switch {
		case r.Method == http.MethodGet:
			// A dropped WATCH ends with its upstream request cancelled mid-body; the proxy then aborts
			// the browser's connection, exactly as a network failure would.
			forward, done := host.forwardGet(r)
			defer done()
			r = forward
		case r.Method == http.MethodPatch && writable:
			// Only a JSON merge patch: a JSON Patch or an apply patch has semantics the check below
			// does not cover.
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				refuse(w, http.StatusUnsupportedMediaType, "only application/merge-patch+json is forwarded")
				return
			}
			patch, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
			if err != nil {
				refuse(w, http.StatusRequestEntityTooLarge, "patch too large")
				return
			}
			if err := gateway.ValidateNativeMergePatch(patch); err != nil {
				refuse(w, http.StatusBadRequest, err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(patch))
			r.ContentLength = int64(len(patch))
			host.mu.Lock()
			hook := host.beforePatch
			host.beforePatch = nil
			host.mu.Unlock()
			if hook != nil {
				hook()
			}
		default:
			refuse(w, http.StatusMethodNotAllowed, "not proxied")
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		host.CloseClientConnections()
		host.Close()
	})
	return host
}

// refuse answers as the API server would, with a Status the editor can show.
func refuse(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Code:     int32(code), //nolint:gosec // an HTTP status code
		Message:  message,
	})
}

type nativeMember struct {
	UID             string            `json:"uid"`
	Name            string            `json:"name"`
	Kind            string            `json:"kind"`
	ResourceVersion string            `json:"resourceVersion"`
	Data            map[string]string `json:"data"`
}

// nativeLine is one line the driver prints: a connection state, a state event with what the store
// then holds, a save's outcome, or an error.
type nativeLine struct {
	State        string         `json:"state"`
	Event        string         `json:"event"`
	Members      []nativeMember `json:"members"`
	Saved        string         `json:"saved"`
	CommandError string         `json:"commandError"`
	Error        *struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Terminal bool   `json:"terminal"`
	} `json:"error"`
}

type nativeDriver struct {
	t       *testing.T
	stdin   io.WriteCloser
	lines   chan nativeLine
	members []nativeMember
	// What await has read so far: each state in order, how often each state event arrived, and the
	// connector errors a test declared it expects (by code, with how many more are allowed).
	states   []string
	events   map[string]int
	expected map[string]int
	errors   []string
}

// newNativeDriver starts the browser side on the collection that scope names behind proxy.
func newNativeDriver(t *testing.T, proxy string, scope map[string]string) *nativeDriver {
	t.Helper()
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node runs the browser connector in this test: %v", err)
	}
	cmd := exec.Command(node, filepath.Join("..", "..", "packages", "krm-stream", "e2e", "native-driver.ts"), proxy, string(scopeJSON)) //nolint:gosec // a fixed path in this repository
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
	d := &nativeDriver{
		t: t, stdin: stdin, lines: make(chan nativeLine, 256), events: map[string]int{}, expected: map[string]int{},
	}
	go func() {
		defer close(d.lines)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			var line nativeLine
			if json.Unmarshal(scanner.Bytes(), &line) == nil {
				d.lines <- line
			}
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return d
}

// expect allows n more connector errors with this code; any other error still fails the test.
func (d *nativeDriver) expect(code string, n int) {
	d.expected[code] += n
}

// await reads lines until pred holds, failing on any connector error the test did not expect.
func (d *nativeDriver) await(what string, pred func(nativeLine) bool) nativeLine {
	d.t.Helper()
	deadline := time.After(30 * time.Second)
	for {
		select {
		case line, ok := <-d.lines:
			if !ok {
				d.t.Fatalf("%s: the driver exited", what)
			}
			if line.Error != nil {
				if line.Error.Terminal || d.expected[line.Error.Code] == 0 {
					d.t.Fatalf("%s: connector error %s: %s (terminal %v)", what, line.Error.Code, line.Error.Message, line.Error.Terminal)
				}
				d.expected[line.Error.Code]--
				d.errors = append(d.errors, line.Error.Code+": "+line.Error.Message)
			}
			if line.CommandError != "" {
				d.t.Fatalf("%s: command failed: %s", what, line.CommandError)
			}
			if line.State != "" {
				d.states = append(d.states, line.State)
			}
			if line.Event != "" {
				d.members = line.Members
				d.events[line.Event]++
			}
			if pred(line) {
				return line
			}
		case <-deadline:
			d.t.Fatalf("%s: not reached; the store holds %+v", what, d.members)
		}
	}
}

func (d *nativeDriver) names() []string {
	names := []string{}
	for _, m := range d.members {
		names = append(names, m.Name)
	}
	return names
}

func (d *nativeDriver) member(name string) nativeMember {
	for _, m := range d.members {
		if m.Name == name {
			return m
		}
	}
	d.t.Fatalf("%s is not in the store: %+v", name, d.members)
	return nativeMember{}
}

// configMapFixture changes labelled ConfigMaps in a scratch namespace, failing the test on error.
type configMapFixture struct {
	t   *testing.T
	ctx context.Context
	api typedcorev1.ConfigMapInterface
}

func newConfigMapFixture(t *testing.T) (*configMapFixture, string) {
	t.Helper()
	cs, _ := clients(t)
	ns := scratchNamespace(t, cs)
	return &configMapFixture{t: t, ctx: context.Background(), api: cs.CoreV1().ConfigMaps(ns)}, ns
}

func (f *configMapFixture) create(name, tier, value string) *corev1.ConfigMap {
	f.t.Helper()
	created, err := f.api.Create(f.ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"tier": tier}},
		Data:       map[string]string{"value": value},
	}, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("create %s: %v", name, err)
	}
	return created
}

func (f *configMapFixture) get(name string) *corev1.ConfigMap {
	f.t.Helper()
	current, err := f.api.Get(f.ctx, name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("get %s: %v", name, err)
	}
	return current
}

func (f *configMapFixture) update(name string, change func(*corev1.ConfigMap)) {
	f.t.Helper()
	current := f.get(name)
	change(current)
	if _, err := f.api.Update(f.ctx, current, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("update %s: %v", name, err)
	}
}

func (f *configMapFixture) remove(name string) {
	f.t.Helper()
	if err := f.api.Delete(f.ctx, name, metav1.DeleteOptions{}); err != nil {
		f.t.Fatalf("delete %s: %v", name, err)
	}
}

// hasNames holds for a state event after which the store's members are exactly want, by name.
func hasNames(want ...string) func(nativeLine) bool {
	return func(line nativeLine) bool {
		if line.Event == "" {
			return false
		}
		names := []string{}
		for _, m := range line.Members {
			names = append(names, m.Name)
		}
		return slices.Equal(names, want)
	}
}

func TestRealAPINativeWatchThroughHostProxy(t *testing.T) {
	fx, ns := newConfigMapFixture(t)
	create, update, remove := fx.create, fx.update, fx.remove

	create("a", "web", "1")
	create("b", "web", "1")
	create("c", "db", "1")

	proxy := nativeProxy(t, false)
	d := newNativeDriver(t, proxy.URL+"/k8s", map[string]string{
		"version": "v1", "resource": "configmaps", "namespace": ns, "labelSelector": "tier=web",
	})

	d.await("live", func(line nativeLine) bool { return line.State == "live" })
	if got := d.names(); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("snapshot: want the selector's members a and b, got %v", got)
	}
	if kind := d.member("a").Kind; kind != "ConfigMap" {
		t.Fatalf("a typed list's items carry no kind; the connector must supply ConfigMap, got %q", kind)
	}

	update("a", func(cm *corev1.ConfigMap) { cm.Data["value"] = "2" })
	d.await("a modified", func(line nativeLine) bool {
		return line.Event == "modified" && len(line.Members) > 0 && line.Members[0].Data["value"] == "2"
	})

	remove("b")
	d.await("b deleted", hasNames("a"))

	// A label change that takes a out of the selector arrives as a deletion.
	update("a", func(cm *corev1.ConfigMap) { cm.Labels["tier"] = "db" })
	d.await("a left the selector", hasNames())

	first := create("d", "web", "1")
	d.await("d added", hasNames("d"))
	remove("d")
	second := create("d", "web", "2")
	d.await("d recreated", func(line nativeLine) bool {
		return hasNames("d")(line) && line.Members[0].UID == string(second.UID)
	})
	if first.UID == second.UID {
		t.Fatal("the recreated object kept its UID; the case proves nothing")
	}

	if _, err := io.WriteString(d.stdin, "close\n"); err != nil {
		t.Fatal(err)
	}
	d.await("closed", func(line nativeLine) bool { return line.State == "closed" })
}

func (d *nativeDriver) send(command map[string]any) {
	d.t.Helper()
	line, err := json.Marshal(command)
	if err != nil {
		d.t.Fatal(err)
	}
	if _, err := d.stdin.Write(append(line, '\n')); err != nil {
		d.t.Fatal(err)
	}
}

func (d *nativeDriver) set(name string, path []any, value any) {
	d.t.Helper()
	d.send(map[string]any{"op": "set", "name": name, "path": path, "value": value})
}

// holds waits until the store's members satisfy pred, checking what it already reported first: a
// watch echo can arrive before the save that caused it reports its outcome.
func (d *nativeDriver) holds(what string, pred func([]nativeMember) bool) {
	d.t.Helper()
	if pred(d.members) {
		return
	}
	d.await(what, func(line nativeLine) bool { return line.Event != "" && pred(line.Members) })
}

// save asks the browser's native editor to save and returns its outcome.
func (d *nativeDriver) save(name string) string {
	d.t.Helper()
	d.send(map[string]any{"op": "save", "name": name})
	return d.await("save "+name, func(line nativeLine) bool { return line.Saved != "" }).Saved
}

func TestRealAPINativeEditThroughHostProxy(t *testing.T) {
	cs, _ := clients(t)
	ns := scratchNamespace(t, cs)
	ctx := context.Background()
	configMaps := cs.CoreV1().ConfigMaps(ns)
	const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"
	const applied = `{"apiVersion":"v1","kind":"ConfigMap","data":{"mode":"a"}}`
	// Safe to call from the proxy's goroutine: it reports with Errorf and returns nil.
	create := func(mode string) *corev1.ConfigMap {
		created, err := configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "settings",
				Annotations: map[string]string{lastApplied: applied, "owner": "team-a"},
			},
			Data: map[string]string{"mode": mode, "other": "1"},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Errorf("create settings: %v", err)
		}
		return created
	}
	get := func() *corev1.ConfigMap {
		t.Helper()
		current, err := configMaps.Get(ctx, "settings", metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get settings: %v", err)
		}
		return current
	}
	echoed := func(mode string) func([]nativeMember) bool {
		return func(members []nativeMember) bool { return len(members) == 1 && members[0].Data["mode"] == mode }
	}
	original := create("a")
	if original == nil {
		t.FailNow()
	}

	proxy := nativeProxy(t, true)
	// One object watched by name, as an editor page for it would: it can be deleted and recreated
	// like any collection member.
	d := newNativeDriver(t, proxy.URL+"/k8s", map[string]string{
		"version": "v1", "resource": "configmaps", "namespace": ns, "name": "settings",
	})
	d.await("live", func(line nativeLine) bool { return line.State == "live" })

	// A save goes through the proxy as a conditional merge patch, and the watch echoes it. The
	// last-applied annotation the store holds is never part of it.
	d.set("settings", []any{"data", "mode"}, "b")
	d.set("settings", []any{"metadata", "annotations", "owner"}, "team-b")
	if got := d.save("settings"); got != "saved" {
		t.Fatalf("first save: want saved, got %s", got)
	}
	d.holds("first save echoed", echoed("b"))
	if cm := get(); cm.Data["mode"] != "b" || cm.Data["other"] != "1" || cm.Annotations["owner"] != "team-b" ||
		cm.Annotations[lastApplied] != applied {
		t.Fatalf("first save: unexpected object %+v %+v", cm.Data, cm.Annotations)
	}

	// Another writer lands between the browser capturing its save and Kubernetes checking it. The
	// stale version is refused, a guarded native read recovers the draft, and the next deliberate
	// save lands: neither write is lost.
	d.set("settings", []any{"data", "mode"}, "c")
	proxy.nextPatch(func() {
		current, err := configMaps.Get(ctx, "settings", metav1.GetOptions{})
		if err == nil {
			current.Data["other"] = "2"
			_, err = configMaps.Update(ctx, current, metav1.UpdateOptions{})
		}
		if err != nil {
			t.Errorf("competing update: %v", err)
		}
	})
	outcomes := []string{d.save("settings")}
	// The watch can apply the competing write before or after the guarded read's capture, so the
	// first recovery either establishes the new base (version-stale) or owes another read.
	for outcomes[len(outcomes)-1] != "saved" {
		if last := outcomes[len(outcomes)-1]; (last != "version-stale" && last != "recovering") || len(outcomes) > 4 {
			t.Fatalf("recovery from a stale version: outcomes %v", outcomes)
		}
		outcomes = append(outcomes, d.save("settings"))
	}
	t.Logf("recovery from a stale version: outcomes %v", outcomes)
	if outcomes[0] == "saved" {
		t.Fatal("the competing write did not make the captured save stale; the case proves nothing")
	}
	d.holds("second save echoed", echoed("c"))
	if cm := get(); cm.Data["mode"] != "c" || cm.Data["other"] != "2" || cm.Annotations[lastApplied] != applied {
		t.Fatalf("after recovery: want mode c and the competing other=2, got %+v %+v", cm.Data, cm.Annotations)
	}

	// The host forwards only conditional patches that leave machinery alone.
	objectURL := proxy.URL + "/k8s/api/v1/namespaces/" + url.PathEscape(ns) + "/configmaps/settings"
	rv := get().ResourceVersion
	for _, patch := range []string{
		`{"data":{"mode":"blind"}}`,
		`{"metadata":{"uid":"` + string(original.UID) + `","resourceVersion":"` + rv + `","annotations":{"` + lastApplied + `":"{}"}}}`,
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, objectURL, strings.NewReader(patch))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/merge-patch+json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("patch %s: want the host to refuse it with 400, got %d", patch, res.StatusCode)
		}
	}
	if cm := get(); cm.ResourceVersion != rv {
		t.Fatalf("a refused patch reached the API server: resourceVersion %s → %s", rv, cm.ResourceVersion)
	}

	// The object is replaced under the same name between capture and write. The old draft never
	// lands on the replacement.
	d.set("settings", []any{"data", "mode"}, "meant for the original")
	replaced := make(chan *corev1.ConfigMap, 1)
	proxy.nextPatch(func() {
		if err := configMaps.Delete(ctx, "settings", metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete settings: %v", err)
		}
		replaced <- create("replacement")
	})
	if got := d.save("settings"); got != "unavailable" {
		t.Fatalf("save to a replaced object: want unavailable, got %s", got)
	}
	var replacement *corev1.ConfigMap
	select {
	case replacement = <-replaced:
	default:
	}
	if replacement == nil || replacement.UID == original.UID {
		t.Fatal("the object was not replaced; the case proves nothing")
	}
	if cm := get(); cm.UID != replacement.UID || cm.Data["mode"] != "replacement" {
		t.Fatalf("the replacement was written: %+v", cm.Data)
	}
	d.holds("replacement observed", func(members []nativeMember) bool {
		return len(members) == 1 && members[0].UID == string(replacement.UID)
	})

	if got, want := proxy.answered(), []int{http.StatusOK, http.StatusConflict, http.StatusOK, http.StatusConflict}; !slices.Equal(got, want) {
		t.Fatalf("forwarded PATCH answers: want %v, got %v", want, got)
	}

	if _, err := io.WriteString(d.stdin, "close\n"); err != nil {
		t.Fatal(err)
	}
	d.await("closed", func(line nativeLine) bool { return line.State == "closed" })
}

// waitClosed waits for ch to be closed.
func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(30 * time.Second):
		t.Fatalf("%s: not reached", what)
	}
}

// The browser resumes a dropped WATCH from its checkpoint through the same host proxy instead of
// listing again: what changed while it was disconnected — an update, a delete, a selector exit and a
// same-name replacement — arrives as watch events, with no new LIST and no reset, and a draft made
// before the drop survives and saves. Then the proxy rewrites the next WATCH's resourceVersion to
// "1": the expiry is induced, but the 410 is the real API server's own in-stream answer, and the
// browser recovers with a fresh LIST — once, then with two expiries in a row.
func TestRealAPINativeResumeThroughHostProxy(t *testing.T) {
	fx, ns := newConfigMapFixture(t)
	fx.create("a", "web", "1")
	fx.create("b", "web", "1")
	fx.create("c", "db", "1")
	fx.create("d", "web", "1")
	original := fx.create("r", "web", "first")

	proxy := nativeProxy(t, true)
	collection := "/k8s/api/v1/namespaces/" + ns + "/configmaps"
	d := newNativeDriver(t, proxy.URL+"/k8s", map[string]string{
		"version": "v1", "resource": "configmaps", "namespace": ns, "labelSelector": "tier=web",
	})
	counts := func(when string, wantLists, wantWatches int) {
		t.Helper()
		if lists, watches := proxy.lists(collection); lists != wantLists || watches != wantWatches {
			t.Fatalf("%s: want %d LISTs and %d WATCHes forwarded, got %d and %d", when, wantLists, wantWatches, lists, watches)
		}
	}
	started := time.Now()
	d.await("live", func(line nativeLine) bool { return line.State == "live" })
	if got := d.names(); !slices.Equal(got, []string{"a", "b", "d", "r"}) {
		t.Fatalf("snapshot: want a, b, d and r, got %v", got)
	}
	counts("snapshot", 1, 1)

	// A draft made before the interruption, on a field nobody else writes.
	d.set("a", []any{"data", "note"}, "mine")

	// Drop the watch, and hold its resumption while the cluster changes underneath.
	held, release := proxy.holdNextWatch()
	if n := proxy.dropWatches(); n != 1 {
		t.Fatalf("dropped %d watches, want 1", n)
	}
	waitClosed(t, held, "the resumed watch")
	mark := len(d.states)
	resumeStarted := time.Now()
	fx.update("a", func(cm *corev1.ConfigMap) { cm.Data["value"] = "2" })
	fx.remove("b")
	// A label change that takes d out of the selector arrives as a deletion.
	fx.update("d", func(cm *corev1.ConfigMap) { cm.Labels["tier"] = "db" })
	fx.remove("r")
	replacement := fx.create("r", "web", "second")
	release()
	d.await("missed changes replayed", func(line nativeLine) bool {
		return hasNames("a", "r")(line) && line.Members[0].Data["value"] == "2" &&
			line.Members[1].UID == string(replacement.UID)
	})
	resumeTook := time.Since(resumeStarted)
	if original.UID == replacement.UID {
		t.Fatal("the recreated object kept its UID; the case proves nothing")
	}
	counts("resume", 1, 2)
	if resumed := proxy.forwardedWatches()[1]; resumed.Asked == "" || resumed.Sent != resumed.Asked ||
		resumed.Answer != http.StatusOK {
		t.Fatalf("the resumed watch: %+v", resumed)
	}
	if d.events["reset"] != 1 || d.events["synced"] != 1 {
		t.Fatalf("a resumed watch starts no snapshot: events %v", d.events)
	}
	if got := d.states[mark:]; !slices.Equal(got, []string{"retrying", "connecting", "live"}) {
		t.Fatalf("states while resuming: want retrying, connecting, live; got %v", got)
	}

	// The draft survived the resume, and saves at the version the resumed watch delivered.
	if got := d.save("a"); got != "saved" {
		t.Fatalf("save after the resume: want saved, got %s", got)
	}
	d.holds("save echoed", func(members []nativeMember) bool {
		return len(members) > 0 && members[0].Name == "a" && members[0].Data["note"] == "mine"
	})
	if cm := fx.get("a"); cm.Data["value"] != "2" || cm.Data["note"] != "mine" {
		t.Fatalf("after the save: want value 2 and note mine, got %v", cm.Data)
	}

	// Expiry: the resumed WATCH asks for a resourceVersion long compacted away. The API server
	// accepts it and reports 410 in the stream; the browser discards its checkpoint and lists.
	proxy.expireNextWatches(1)
	held, release = proxy.holdNextWatch()
	mark = len(d.states)
	expiryStarted := time.Now()
	proxy.dropWatches()
	waitClosed(t, held, "the expiring watch")
	fx.create("f", "web", "1")
	release()
	d.expect("RESYNC_REQUIRED", 1)
	d.await("expiry recovered", func(line nativeLine) bool { return d.events["reset"] == 2 && line.State == "live" })
	expiryTook := time.Since(expiryStarted)
	if got := d.names(); !slices.Equal(got, []string{"a", "f", "r"}) {
		t.Fatalf("after the replacement snapshot: want a, f and r, got %v", got)
	}
	counts("expiry", 2, 4)
	expiredWatch := proxy.forwardedWatches()[2]
	if expiredWatch.Sent != "1" || expiredWatch.Asked == "1" || expiredWatch.Answer != http.StatusOK {
		t.Fatalf("the expired watch: want an accepted watch from resourceVersion 1, got %+v", expiredWatch)
	}
	if len(d.errors) != 1 {
		t.Fatalf("want one RESYNC_REQUIRED, got %v", d.errors)
	}
	// The expired watch is accepted before its ERROR arrives, so it is briefly live; after the 410 the
	// state is never live again until the replacement snapshot completes.
	if got, want := d.states[mark:], []string{"retrying", "connecting", "live", "retrying", "connecting", "syncing", "syncing", "live"}; !slices.Equal(got, want) {
		t.Fatalf("states through expiry: want %v, got %v", want, got)
	}

	// Two expiries in a row: the resumed WATCH and the replacement snapshot's WATCH. Each one costs
	// a retry with backoff, and the third snapshot converges.
	proxy.expireNextWatches(2)
	proxy.dropWatches()
	fx.update("a", func(cm *corev1.ConfigMap) { cm.Data["value"] = "3" })
	d.expect("RESYNC_REQUIRED", 2)
	d.await("repeated expiry recovered", func(line nativeLine) bool {
		return d.events["reset"] == 4 && line.State == "live"
	})
	d.holds("a converged", func(members []nativeMember) bool {
		return len(members) == 3 && members[0].Name == "a" && members[0].Data["value"] == "3"
	})
	counts("repeated expiry", 4, 7)
	watches := proxy.forwardedWatches()
	if watches[4].Sent != "1" || watches[5].Sent != "1" || watches[6].Sent != watches[6].Asked {
		t.Fatalf("repeated expiry: want watches 5 and 6 expired and 7 not, got %+v", watches[4:])
	}
	if len(d.errors) != 3 {
		t.Fatalf("want three RESYNC_REQUIRED, got %v", d.errors)
	}

	t.Logf("resume: %d LISTs, %d WATCHes; resumed from resourceVersion %q, replay converged in %v; "+
		"expiry recovery %v; errors %v; total %v",
		4, len(watches), proxy.forwardedWatches()[1].Asked, resumeTook.Round(time.Millisecond),
		expiryTook.Round(time.Millisecond), d.errors, time.Since(started).Round(time.Millisecond))

	if _, err := io.WriteString(d.stdin, "close\n"); err != nil {
		t.Fatal(err)
	}
	d.await("closed", func(line nativeLine) bool { return line.State == "closed" })
}
