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
	host := &nativeHost{}
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
			if res.Request.Method == http.MethodPatch {
				host.mu.Lock()
				host.patches = append(host.patches, res.StatusCode)
				host.mu.Unlock()
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
	d := &nativeDriver{t: t, stdin: stdin, lines: make(chan nativeLine, 256)}
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

// await reads lines until pred holds, failing on any error the connector reports.
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
				d.t.Fatalf("%s: connector error %s: %s (terminal %v)", what, line.Error.Code, line.Error.Message, line.Error.Terminal)
			}
			if line.CommandError != "" {
				d.t.Fatalf("%s: command failed: %s", what, line.CommandError)
			}
			if line.Event != "" {
				d.members = line.Members
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

func TestRealAPINativeWatchThroughHostProxy(t *testing.T) {
	cs, _ := clients(t)
	ns := scratchNamespace(t, cs)
	ctx := context.Background()
	configMaps := cs.CoreV1().ConfigMaps(ns)
	create := func(name, tier, value string) *corev1.ConfigMap {
		t.Helper()
		created, err := configMaps.Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"tier": tier}},
			Data:       map[string]string{"value": value},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return created
	}
	update := func(name string, change func(*corev1.ConfigMap)) {
		t.Helper()
		current, err := configMaps.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", name, err)
		}
		change(current)
		if _, err := configMaps.Update(ctx, current, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("update %s: %v", name, err)
		}
	}
	remove := func(name string) {
		t.Helper()
		if err := configMaps.Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	hasNames := func(want ...string) func(nativeLine) bool {
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
