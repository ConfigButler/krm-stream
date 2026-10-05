//go:build e2e

// Native viewing against a real API server: the browser's connectNativeWatch, run by node
// (packages/krm-stream/e2e/native-driver.ts), through a host-style /k8s proxy that holds the
// credentials. No gateway is involved; this checks that a real LIST and WATCH — typed collections
// whose items carry no apiVersion or kind, selector exits, deletes and same-name recreation — reach
// the read-only store as the connector promises.
//
//	task test-real-api
package kube_test

import (
	"bufio"
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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// nativeProxy is the host: it serves the API server's paths under /k8s with its own credentials, for
// GET only. The browser never holds a Kubernetes credential.
func nativeProxy(t *testing.T) *httptest.Server {
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
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/k8s/") {
			http.Error(w, "not proxied", http.StatusNotFound)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
	})
	return server
}

type nativeMember struct {
	UID  string            `json:"uid"`
	Name string            `json:"name"`
	Kind string            `json:"kind"`
	Data map[string]string `json:"data"`
}

// nativeLine is one line the driver prints: a connection state, a state event with what the store
// then holds, or an error.
type nativeLine struct {
	State   string         `json:"state"`
	Event   string         `json:"event"`
	Members []nativeMember `json:"members"`
	Error   *struct {
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

func newNativeDriver(t *testing.T, collectionURL string) *nativeDriver {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatalf("node runs the browser connector in this test: %v", err)
	}
	cmd := exec.Command(node, filepath.Join("..", "..", "packages", "krm-stream", "e2e", "native-driver.ts"), collectionURL) //nolint:gosec // a fixed path in this repository
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

	proxy := nativeProxy(t)
	d := newNativeDriver(t, proxy.URL+"/k8s/api/v1/namespaces/"+url.PathEscape(ns)+"/configmaps?labelSelector=tier%3Dweb")

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
