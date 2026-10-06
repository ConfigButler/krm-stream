package comparison

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
)

// No cluster here: the schedule, the native proxy's policy and counters, and the gateway routes'
// refusal are checked against stand-ins. The real-API runs are the measurement itself
// (examples/comparison/measure.ts).

func TestPlanIsDeterministicAndShaped(t *testing.T) {
	for _, name := range Workloads {
		req := WorkloadRequest{Name: name, DurationMs: 20000, Seed: 7}
		a, err := NewPlan(req, 8, 3)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := NewPlan(req, 8, 3)
		if !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: the same request planned two different schedules", name)
		}
		counts := a.Counts()
		if counts[StepDisconnect] < 2 {
			t.Errorf("%s: %d forced reconnects, want at least 2 in every workload", name, counts[StepDisconnect])
		}
		for i := 1; i < len(a.Steps); i++ {
			if a.Steps[i].At < a.Steps[i-1].At {
				t.Fatalf("%s: steps out of order", name)
			}
		}
		for _, s := range a.Steps {
			if s.At <= 0 || s.At >= 20*time.Second+time.Second {
				t.Fatalf("%s: step at %v is outside the run", name, s.At)
			}
		}
		switch name {
		case WorkloadQuiet:
			if len(a.Steps) != counts[StepDisconnect] {
				t.Errorf("quiet writes: %v", counts)
			}
		case WorkloadChurn:
			// Status-only churn dominates: that is what krm-spec/v1 suppresses.
			if counts[StepStatus] < 150 || counts[StepSpec] == 0 || counts[StepSecret] == 0 || counts[StepStatus] < 10*counts[StepSpec] {
				t.Errorf("churn shape: %v", counts)
			}
		case WorkloadBurst:
			if counts[StepSpec]+counts[StepStatus] != 5*12 || counts[StepSecret] != 0 {
				t.Errorf("burst shape: %v", counts)
			}
		case WorkloadForcedDisconnect:
			if counts[StepDisconnect] < 2+4 {
				t.Errorf("forced-disconnect has %d disconnects", counts[StepDisconnect])
			}
		}
	}
	other, _ := NewPlan(WorkloadRequest{Name: WorkloadChurn, DurationMs: 20000, Seed: 8}, 8, 3)
	same, _ := NewPlan(WorkloadRequest{Name: WorkloadChurn, DurationMs: 20000, Seed: 7}, 8, 3)
	if reflect.DeepEqual(other.Steps, same.Steps) {
		t.Error("a different seed planned the same objects")
	}
	if _, err := NewPlan(WorkloadRequest{Name: "storm"}, 8, 3); err == nil {
		t.Error("an unknown workload was planned")
	}
	if _, err := NewPlan(WorkloadRequest{Name: WorkloadQuiet, Target: "everything"}, 8, 3); err == nil {
		t.Error("an unknown disconnect target was accepted")
	}
}

func testHost() *Host {
	return &Host{
		cfg:              Config{ViewerToken: "v", RefusedToken: "r"},
		namespace:        "ns",
		metrics:          NewMetrics(),
		connections:      newConnections(),
		reauthorizeEvery: time.Hour,
	}
}

func TestNativeProxyPolicyAndCounters(t *testing.T) {
	var upstream atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "1" {
			_, _ = w.Write([]byte(`{"type":"ADDED","object":{}}` + "\n" + `{"type":"BOOKMARK","object":{}}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"kind":"SecretList","metadata":{"resourceVersion":"5"},"items":[]}`))
	}))
	defer api.Close()
	h := testHost()
	target, _ := url.Parse(api.URL)
	proxy := newNativeProxy(h, target, http.DefaultTransport)
	collection := "/k8s/api/v1/namespaces/ns/secrets"

	do := func(method, path, session, body string) int {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if session != "" {
			r.Header.Set(SessionHeader, session)
		}
		if method == http.MethodPatch {
			r.Header.Set("Content-Type", "application/merge-patch+json")
		}
		w := httptest.NewRecorder()
		proxy.ServeHTTP(w, r)
		return w.Code
	}

	// Refusals never reach the API server.
	for _, c := range []struct {
		method, path, session, body string
		want                        int
	}{
		{http.MethodGet, collection, "", "", http.StatusUnauthorized},
		{http.MethodGet, collection, "r", "", http.StatusForbidden},
		{http.MethodGet, "/k8s/api/v1/namespaces/other/secrets", "v", "", http.StatusForbidden},
		{http.MethodGet, collection + "/s-0/status", "v", "", http.StatusForbidden},
		{http.MethodDelete, collection + "/s-0", "v", "", http.StatusMethodNotAllowed},
		{http.MethodPatch, collection + "/s-0", "v", `{"metadata":{"uid":"u","resourceVersion":"1"}}`, http.StatusMethodNotAllowed},
		{http.MethodPatch, "/k8s/apis/" + WidgetGroup + "/v1/namespaces/ns/widgets/w-00", "v", `{"spec":{"note":"x"}}`, http.StatusBadRequest},
	} {
		if got := do(c.method, c.path, c.session, c.body); got != c.want {
			t.Errorf("%s %s as %q: %d, want %d", c.method, c.path, c.session, got, c.want)
		}
	}
	if n := upstream.Load(); n != 0 {
		t.Fatalf("%d refused requests reached the API server", n)
	}

	// A LIST then its WATCH, then a WATCH with no LIST before it: a resumed watch.
	do(http.MethodGet, collection, "v", "")
	do(http.MethodGet, collection+"?watch=1&resourceVersion=5", "v", "")
	do(http.MethodGet, collection+"?watch=1&resourceVersion=9", "v", "")
	c := h.metrics.Snapshot().Counters
	want := map[string]int64{
		"native.list_requests": 1, "native.watch_requests": 2, "native.watch_after_list": 1, "native.watch_resumed": 1,
		"native.watch_frames.ADDED": 2, "native.watch_frames.BOOKMARK": 2, "native.refused_session_requests": 1,
	}
	for key, n := range want {
		if c[key] != n {
			t.Errorf("%s = %d, want %d (all: %v)", key, c[key], n, c)
		}
	}
	if c["native.bytes.watch"] == 0 || c["native.bytes.list"] == 0 {
		t.Errorf("downstream bytes not counted: %v", c)
	}
	if g := h.metrics.Snapshot().Gauges["native.watches_active"]; g != 0 {
		t.Errorf("native.watches_active = %d after every watch ended", g)
	}
}

// stubBackend serves one object and a completed snapshot, then holds the watch open.
type stubBackend struct{ watches atomic.Int64 }

func (b *stubBackend) Watch(_ context.Context, _ gateway.Scope) (gateway.Watcher, error) {
	b.watches.Add(1)
	return &stubWatcher{events: []gateway.WatchEvent{
		{Type: gateway.WatchAdded, Object: gateway.KRMObject{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
			"metadata": map[string]any{"name": "s-0", "namespace": "ns", "uid": "u1", "resourceVersion": "3"},
			"data":     map[string]any{"token": "c2VjcmV0"}, //nolint:gosec // a test value, base64 of "secret"
		}},
		{Type: gateway.WatchBookmark, InitialEventsEnd: true},
	}}, nil
}

type stubWatcher struct{ events []gateway.WatchEvent }

func (w *stubWatcher) Next(ctx context.Context) (gateway.WatchEvent, error) {
	if len(w.events) > 0 {
		ev := w.events[0]
		w.events = w.events[1:]
		return ev, nil
	}
	<-ctx.Done()
	return gateway.WatchEvent{}, ctx.Err()
}

func (w *stubWatcher) Stop() {}

func TestGatewayRouteRefusesAndDisconnects(t *testing.T) {
	h := testHost()
	backend := &stubBackend{}
	counted := &countingBackend{inner: backend, metrics: h.metrics, route: "full"}
	server := httptest.NewServer(h.streamRoute(Routes[0], counted))
	defer server.Close()
	streamURL := server.URL + "?version=v1&resource=secrets&namespace=ns"

	open := func(session string) (*http.Response, *bufio.Reader) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set(SessionHeader, session)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res, bufio.NewReader(res.Body)
	}
	frame := func(r *bufio.Reader) string {
		t.Helper()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return ""
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				return data
			}
		}
	}

	// The refused session: a terminal FORBIDDEN, and no upstream watch was opened for it.
	res, r := open("r")
	if got := frame(r); !strings.Contains(got, `"FORBIDDEN"`) || !strings.Contains(got, `"terminal":true`) {
		t.Fatalf("refused session got %s", got)
	}
	_ = res.Body.Close()
	if n := backend.watches.Load(); n != 0 {
		t.Fatalf("a refused session opened %d upstream watches", n)
	}
	// The terminal frame is flushed before the handler returns and releases its connection, so
	// wait for that release; otherwise the forced disconnect below can still count it.
	deadline := time.Now().Add(5 * time.Second)
	for openConnections(h.connections, "gateway") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the refused session's connection was never released")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The viewer: a projected snapshot. The Secret's value is withheld, and its path is named.
	res, r = open("v")
	defer func() { _ = res.Body.Close() }()
	frame(r) // reset
	added := frame(r)
	if strings.Contains(added, "c2VjcmV0") || !strings.Contains(added, `"path":"/data/token"`) {
		t.Fatalf("full projection leaked or lost the redaction: %s", added)
	}
	if !strings.Contains(frame(r), `"synced"`) {
		t.Fatal("no synced")
	}
	if g := h.metrics.Snapshot().Gauges["gateway.full.upstream_watches_active"]; g != 1 {
		t.Fatalf("upstream_watches_active = %d, want 1", g)
	}

	// A forced disconnect ends the stream.
	if n := h.connections.cancel("gateway"); n != 1 {
		t.Fatalf("closed %d gateway connections, want 1", n)
	}
	if _, err := r.ReadString(0); err == nil {
		t.Fatal("the stream survived a forced disconnect")
	}
	deadline = time.Now().Add(5 * time.Second)
	for h.metrics.Snapshot().Gauges["gateway.full.upstream_watches_active"] != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upstream watch was not stopped after its stream ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := h.metrics.Snapshot().Counters
	if c["gateway.full.authorizer_refusals"] != 1 || c["sessions.gateway.refused"] != 1 || c["gateway.full.upstream_watch_calls"] != 1 {
		t.Fatalf("counters: %v", c)
	}
}

// openConnections reports how many connections of kind are still registered.
func openConnections(c *connections, kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, conn := range c.conns {
		if conn.kind == kind {
			n++
		}
	}
	return n
}
