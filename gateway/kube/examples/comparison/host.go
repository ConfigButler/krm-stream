// Package comparison is a host that serves ONE set of objects through every source krm-stream
// offers, from one origin, so a page and a measurement driver can compare them under identical
// objects and identical churn:
//
//   - native: the API server's own LIST and WATCH through a credential-holding /k8s proxy, and
//     conditional merge PATCHes back through it;
//   - gateway: krm-full/v1 and krm-spec/v1 streams, each unshared and shared, with host save
//     endpoints for projected editing.
//
// It owns one scratch namespace, a fixed set of Widgets (a CRD with a status subresource) and
// Secrets, a deterministic workload runner, a forced-disconnect switch and low-cardinality counters.
// It is an example and a measurement harness, not a library API; see examples/comparison/README.md.
package comparison

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
	"github.com/ConfigButler/krm-stream/gateway/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// The two sessions this host knows. A real host resolves sessions from its own store; these are
// fixed so a page and a driver can use them without a login system.
const (
	sessionViewer  = "viewer"
	sessionRefused = "refused"
	// SessionCookie carries the page's session; SessionHeader carries a driver's, or a page panel
	// that deliberately opens a second identity.
	SessionCookie = "krm-compare-session"
	SessionHeader = "X-Compare-Session"
)

// Config configures a Host.
type Config struct {
	// Cluster is the host's own identity. Every upstream request uses it.
	Cluster *rest.Config
	// NamespacePrefix names the scratch namespace, created by New and deleted by Close.
	NamespacePrefix string
	// Widgets and Secrets size the fixed object set. Defaults 8 and 3.
	Widgets, Secrets int
	// ViewerToken and RefusedToken are the session values the host accepts. The viewer may use
	// every source; the refused session is authenticated but every source refuses it.
	ViewerToken, RefusedToken string
	// ReauthorizeEvery is the gateway's timed recheck interval. Default 5s: short, so a 20-second
	// workload shows the per-subscriber cost of timed checks between its forced reconnects.
	ReauthorizeEvery time.Duration
	// Dist is the built library directory served at /packages/krm-stream/dist/. Page is
	// examples/comparison, served at /examples/comparison/.
	Dist, Page string
}

// Host is the comparison host.
type Host struct {
	cfg              Config
	namespace        string
	core             kubernetes.Interface
	objects          *objects
	metrics          *Metrics
	connections      *connections
	reauthorizeEvery time.Duration
	mux              *http.ServeMux
	workload         sync.Mutex
}

// New creates the scratch namespace and the objects, and builds every route. Call Close to delete
// the namespace.
func New(ctx context.Context, cfg Config) (*Host, error) {
	if cfg.Cluster == nil {
		return nil, errors.New("comparison: a cluster configuration is required")
	}
	if cfg.Widgets <= 0 {
		cfg.Widgets = 8
	}
	if cfg.Secrets <= 0 {
		cfg.Secrets = 3
	}
	if cfg.NamespacePrefix == "" {
		cfg.NamespacePrefix = "krm-compare"
	}
	if cfg.ViewerToken == "" || cfg.RefusedToken == "" || cfg.ViewerToken == cfg.RefusedToken {
		return nil, errors.New("comparison: two distinct session tokens are required")
	}
	if cfg.ReauthorizeEvery <= 0 {
		cfg.ReauthorizeEvery = 5 * time.Second
	}
	// A declared fixture capacity, not a library default: client-go's own default (5 QPS) would
	// throttle ten unshared streams reconnecting at once, and the measurement would show the limiter.
	cluster := rest.CopyConfig(cfg.Cluster)
	cluster.QPS, cluster.Burst = 200, 400

	httpClient, err := kube.HTTPClientFor(cluster)
	if err != nil {
		return nil, err
	}
	core, err := kubernetes.NewForConfigAndClient(cluster, httpClient)
	if err != nil {
		return nil, err
	}
	writer, err := dynamic.NewForConfigAndClient(cluster, httpClient)
	if err != nil {
		return nil, err
	}
	if err := ensureWidgetCRD(ctx, writer); err != nil {
		return nil, err
	}
	ns, err := core.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{GenerateName: cfg.NamespacePrefix + "-"},
	}, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("create the scratch namespace: %w", err)
	}
	h := &Host{
		cfg:              cfg,
		namespace:        ns.Name,
		core:             core,
		objects:          &objects{dyn: writer, namespace: ns.Name, widgets: cfg.Widgets, secrets: cfg.Secrets},
		metrics:          NewMetrics(),
		connections:      newConnections(),
		reauthorizeEvery: cfg.ReauthorizeEvery,
	}
	if err := h.objects.create(ctx); err != nil {
		_ = h.Close(context.Background())
		return nil, err
	}
	if err := h.routes(cluster); err != nil {
		_ = h.Close(context.Background())
		return nil, err
	}
	return h, nil
}

// Namespace is the scratch namespace.
func (h *Host) Namespace() string { return h.namespace }

// Close closes every downstream connection and deletes the scratch namespace.
func (h *Host) Close(ctx context.Context) error {
	h.connections.cancel("all")
	err := h.core.CoreV1().Namespaces().Delete(ctx, h.namespace, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("delete namespace %s: %w", h.namespace, err)
	}
	return nil
}

// ServeHTTP serves every route.
func (h *Host) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Host) routes(cluster *rest.Config) error {
	mux := http.NewServeMux()

	// Native: the proxy forwards with the host's transport, counted only for the bytes it reads
	// back from the API server.
	transport, err := rest.TransportFor(cluster)
	if err != nil {
		return err
	}
	target, err := url.Parse(cluster.Host)
	if err != nil {
		return err
	}
	native := newNativeProxy(h, target, countingTransport(transport, h.metrics, "native.upstream_bytes"))
	mux.Handle("/k8s/", native)

	// Gateway: one kube.Backend per route, each on its own client so each route's upstream bytes
	// are its own.
	for _, route := range Routes {
		routeConfig := rest.CopyConfig(cluster)
		key := "gateway." + route.Name + ".upstream_bytes"
		routeConfig.Wrap(func(rt http.RoundTripper) http.RoundTripper { return countingTransport(rt, h.metrics, key) })
		client, err := kube.HTTPClientFor(routeConfig)
		if err != nil {
			return err
		}
		dyn, err := dynamic.NewForConfigAndClient(routeConfig, client)
		if err != nil {
			return err
		}
		backend := &countingBackend{inner: kube.NewBackend(dyn), metrics: h.metrics, route: route.Name}
		mux.Handle(route.Path, h.streamRoute(route, backend))
	}

	mux.HandleFunc("/save/", h.saveHandler)

	mux.HandleFunc("GET /login", h.login)
	mux.HandleFunc("GET /config", h.config)
	mux.HandleFunc("GET /metrics", h.admin(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, h.metrics.Snapshot())
	}))
	mux.HandleFunc("POST /metrics/reset", h.admin(func(w http.ResponseWriter, _ *http.Request) {
		h.metrics.Reset()
		writeJSON(w, h.metrics.Snapshot())
	}))
	mux.HandleFunc("POST /admin/disconnect", h.admin(func(w http.ResponseWriter, r *http.Request) {
		target := r.URL.Query().Get("target")
		if target != "native" && target != "gateway" && target != "all" {
			http.Error(w, "target must be native, gateway or all", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]int{"closed": h.disconnect(target)})
	}))
	mux.HandleFunc("POST /admin/workload", h.admin(h.runWorkload))
	mux.HandleFunc("POST /admin/reset", h.admin(func(w http.ResponseWriter, r *http.Request) {
		if err := h.objects.reset(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]bool{"reset": true})
	}))
	mux.HandleFunc("POST /admin/write", h.admin(h.competingWrite))
	mux.HandleFunc("GET /admin/state", h.admin(func(w http.ResponseWriter, r *http.Request) {
		state, err := h.objects.state(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, state)
	}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })

	// The page and the library it imports. The page addresses the library as
	// ../../packages/krm-stream/dist/, like the other examples, so a configurable directory can
	// stand in for the repository's own build.
	if h.cfg.Page != "" {
		mux.Handle("GET /examples/comparison/", http.StripPrefix("/examples/comparison/", staticDir(h.cfg.Page)))
	}
	if h.cfg.Dist != "" {
		mux.Handle("GET /packages/krm-stream/dist/", http.StripPrefix("/packages/krm-stream/dist/", staticDir(h.cfg.Dist)))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	h.mux = mux
	return nil
}

// staticDir serves files, never directory listings, uncached so a rebuilt library is what loads.
// ServeContent rather than a FileServer, which redirects index.html to its directory.
func staticDir(dir string) http.Handler {
	root := http.Dir(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "" {
			path = "index.html"
		}
		// http.Dir confines the name to dir, as a file server does.
		f, err := root.Open("/" + path)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, info.Name(), info.ModTime(), f)
	})
}

// --------------------------------------------------------------------------------- sessions --

type session struct {
	name    string
	refused bool
}

var errNoSession = errors.New("no session")

// sessionOf resolves the caller's session from the header, else the cookie, and counts the check
// under the path that asked.
func (h *Host) sessionOf(r *http.Request, path string) (session, error) {
	token := r.Header.Get(SessionHeader)
	if token == "" {
		if cookie, err := r.Cookie(SessionCookie); err == nil {
			token = cookie.Value
		}
	}
	switch token {
	case h.cfg.ViewerToken:
		h.metrics.Add("sessions."+path+".viewer", 1)
		return session{name: sessionViewer}, nil
	case h.cfg.RefusedToken:
		h.metrics.Add("sessions."+path+".refused", 1)
		return session{name: sessionRefused, refused: true}, nil
	default:
		h.metrics.Add("sessions."+path+".missing", 1)
		return session{}, errNoSession
	}
}

// authenticate answers 401 for a request without a session.
func (h *Host) authenticate(w http.ResponseWriter, r *http.Request, path string) (session, bool) {
	s, err := h.sessionOf(r, path)
	if err != nil {
		refuse(w, http.StatusUnauthorized, "sign in first")
		return session{}, false
	}
	return s, true
}

// admin guards the metrics and admin routes with the viewer session. They are harness controls;
// bind the host to loopback.
func (h *Host) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := h.authenticate(w, r, "admin")
		if !ok {
			return
		}
		if s.refused {
			refuse(w, http.StatusForbidden, "not an administrator")
			return
		}
		next(w, r)
	}
}

// login stands in for the host's sign-in: it sets the viewer session cookie and opens the page.
func (h *Host) login(w http.ResponseWriter, r *http.Request) {
	// Secure on the TLS listener. The plain listener exists for drivers on loopback, where a Secure
	// cookie could not be set.
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: h.cfg.ViewerToken, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode}) //nolint:gosec // see above
	// Only the page's own parameters are passed on.
	query := url.Values{}
	for _, name := range []string{"entry", "variant"} {
		if v := r.URL.Query().Get(name); v == "bundle" || v == "shared" {
			query.Set(name, v)
		}
	}
	target := "/examples/comparison/index.html"
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// PageConfig is what the page needs to address the objects.
type PageConfig struct {
	Namespace    string   `json:"namespace"`
	Group        string   `json:"group"`
	Widgets      []string `json:"widgets"`
	Secrets      []string `json:"secrets"`
	Routes       []Route  `json:"routes"`
	RefusedToken string   `json:"refusedSession"`
	Workloads    []string `json:"workloads"`
}

func (h *Host) config(w http.ResponseWriter, r *http.Request) {
	s, ok := h.authenticate(w, r, "page")
	if !ok {
		return
	}
	if s.refused {
		refuse(w, http.StatusForbidden, "this session may not read these objects")
		return
	}
	c := PageConfig{Namespace: h.namespace, Group: WidgetGroup, Routes: Routes, RefusedToken: h.cfg.RefusedToken, Workloads: Workloads}
	for i := range h.cfg.Widgets {
		c.Widgets = append(c.Widgets, WidgetName(i))
	}
	for i := range h.cfg.Secrets {
		c.Secrets = append(c.Secrets, SecretName(i))
	}
	writeJSON(w, c)
}

// ----------------------------------------------------------------------------------- admin --

func (h *Host) disconnect(target string) int {
	n := h.connections.cancel(target)
	h.metrics.Add("admin.disconnects."+target, 1)
	h.metrics.Add("admin.connections_closed", int64(n))
	return n
}

func (h *Host) runWorkload(w http.ResponseWriter, r *http.Request) {
	var req WorkloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "invalid workload request", http.StatusBadRequest)
		return
	}
	plan, err := NewPlan(req, h.cfg.Widgets, h.cfg.Secrets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.workload.TryLock() {
		http.Error(w, "a workload is already running", http.StatusConflict)
		return
	}
	defer h.workload.Unlock()
	h.metrics.Add("admin.workloads."+plan.Request.Name, 1)
	writeJSON(w, h.objects.run(r.Context(), plan, h.disconnect))
}

// competingWrite changes one Widget's spec.note as another writer would: the page uses it to make
// a conflict with a draft on purpose.
func (h *Host) competingWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Note string `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name and note are required", http.StatusBadRequest)
		return
	}
	if err := h.objects.patch(r.Context(), h.objects.widgetClient(), req.Name, map[string]any{"spec": map[string]any{"note": req.Note}}); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]bool{"written": true})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// ------------------------------------------------------------------------------ connections --

// connections tracks the downstream connections a forced disconnect can close: native watch
// responses and gateway SSE streams.
type connections struct {
	mu    sync.Mutex
	next  int
	conns map[int]openConnection
}

type openConnection struct {
	kind   string
	cancel context.CancelFunc
}

func newConnections() *connections { return &connections{conns: map[int]openConnection{}} }

// open registers a connection of kind and returns its context and its release.
func (c *connections) open(parent context.Context, kind string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	c.mu.Lock()
	id := c.next
	c.next++
	c.conns[id] = openConnection{kind: kind, cancel: cancel}
	c.mu.Unlock()
	return ctx, func() {
		c.mu.Lock()
		delete(c.conns, id)
		c.mu.Unlock()
		cancel()
	}
}

// cancel closes every connection of kind ("all" for every kind) and reports how many.
func (c *connections) cancel(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for id, conn := range c.conns {
		if kind == "all" || conn.kind == kind {
			conn.cancel()
			delete(c.conns, id)
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------- upstream bytes --

// countingTransport counts the response bytes a client reads from the API server.
func countingTransport(rt http.RoundTripper, m *Metrics, key string) http.RoundTripper {
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		res, err := rt.RoundTrip(r)
		if err != nil || res.Body == nil {
			return res, err
		}
		res.Body = &countingBody{ReadCloser: res.Body, add: func(n int64) { m.Add(key, n) }}
		return res, nil
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type countingBody struct {
	io.ReadCloser
	add func(int64)
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.add(int64(n))
	}
	return n, err
}

var _ gateway.Backend = (*countingBackend)(nil)
