package comparison

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	"github.com/ConfigButler/krm-stream/gateway"
)

// The native source: the API server's own paths under /k8s, forwarded with the HOST's credentials.
// The browser never holds a Kubernetes credential. The host decides which paths it forwards (the
// Widgets and Secrets of one namespace), which methods (GET, and a conditional merge PATCH of a
// Widget that gateway.ValidateNativeMergePatch accepts) and for which sessions.
//
// Kubernetes authorizes each forwarded request as the host's identity. That check happens inside the
// API server and is invisible here: these counters see requests, not RBAC decisions.

// nativeProxy forwards /k8s requests and counts them.
type nativeProxy struct {
	host    *Host
	proxy   *httputil.ReverseProxy
	pending pendingLists
}

func newNativeProxy(h *Host, target *url.URL, transport http.RoundTripper) *nativeProxy {
	return &nativeProxy{
		host: h,
		proxy: &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				r.Out.URL.Scheme = target.Scheme
				r.Out.URL.Host = target.Host
				r.Out.Host = target.Host
				r.Out.URL.Path = strings.TrimPrefix(r.In.URL.Path, "/k8s")
				r.Out.URL.RawPath = ""
			},
			Transport: transport,
			// A watch is a long response: every frame must reach the browser as it is written.
			FlushInterval: -1,
			// The default handler logs every forced disconnect; this one only answers.
			ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				refuse(w, http.StatusBadGateway, "the Kubernetes request failed")
			},
		},
		pending: pendingLists{m: map[string]int{}},
	}
}

// pendingLists pairs each WATCH with the LIST before it. The connector LISTs and then WATCHes from
// the collection's resourceVersion; a WATCH that finds no LIST waiting for its collection and session
// was opened without one, which is what a resumed watch looks like from here. The pairing is per
// collection and session, not per browser tab, so it is exact in aggregate rather than per connection.
type pendingLists struct {
	mu sync.Mutex
	m  map[string]int
}

func (p *pendingLists) listed(key string) {
	p.mu.Lock()
	p.m[key]++
	p.mu.Unlock()
}

// take reports whether a LIST was waiting for this WATCH, consuming it.
func (p *pendingLists) take(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m[key] == 0 {
		return false
	}
	p.m[key]--
	return true
}

// nativeTarget is what a /k8s path addresses, or "" when this host does not forward it.
type nativeTarget struct {
	resource string // "widgets" or "secrets"
	name     string // "" for the collection
}

// classify accepts exactly the Widget and Secret collections of the scratch namespace, and their
// members. Everything else, including other namespaces and subresources, is refused.
func (p *nativeProxy) classify(path string) (nativeTarget, bool) {
	ns := p.host.namespace
	for _, c := range []struct{ prefix, resource string }{
		{"/k8s/apis/" + WidgetGroup + "/v1/namespaces/" + ns + "/widgets", "widgets"},
		{"/k8s/api/v1/namespaces/" + ns + "/secrets", "secrets"},
	} {
		if path == c.prefix {
			return nativeTarget{resource: c.resource}, true
		}
		if name, ok := strings.CutPrefix(path, c.prefix+"/"); ok && name != "" && !strings.Contains(name, "/") {
			return nativeTarget{resource: c.resource, name: name}, true
		}
	}
	return nativeTarget{}, false
}

func (p *nativeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m := p.host.metrics
	m.Add("native.session_checks", 1)
	session, ok := p.host.authenticate(w, r, "native")
	if !ok {
		return
	}
	if session.refused {
		// This session may not use either source. The page must never reach here after the gateway
		// refused it; this counter is how the comparison shows that it did not.
		m.Add("native.refused_session_requests", 1)
		refuse(w, http.StatusForbidden, "this session may not read these objects")
		return
	}
	target, ok := p.classify(r.URL.Path)
	if !ok {
		m.Add("native.refused_path", 1)
		refuse(w, http.StatusForbidden, "not proxied by this host")
		return
	}
	query := r.URL.Query()
	writer := &countingWriter{ResponseWriter: w, add: func(int64) {}}
	var class string
	switch {
	case r.Method == http.MethodGet && target.name == "" && isWatch(query):
		class = "watch"
		m.Add("native.watch_requests", 1)
		key := session.name + " " + r.URL.Path + "?" + selectorsOf(query)
		switch {
		case query.Get("sendInitialEvents") == "true":
			m.Add("native.watch_streaming_list", 1)
		case p.pending.take(key):
			m.Add("native.watch_after_list", 1)
		default:
			// No LIST preceded it: a WATCH resumed from a checkpoint the browser kept.
			m.Add("native.watch_resumed", 1)
		}
		if query.Get("resourceVersion") == "" {
			m.Add("native.watch_without_resource_version", 1)
		}
		if query.Get("allowWatchBookmarks") == "true" {
			m.Add("native.watch_bookmarks_requested", 1)
		}
		writer.frames = func(chunk []byte) { countFrames(m, chunk) }
		m.Gauge("native.watches_active", 1)
		defer m.Gauge("native.watches_active", -1)
		// A forced disconnect cancels this request; the proxy then aborts the response, as a dropped
		// connection would.
		ctx, release := p.host.connections.open(r.Context(), "native")
		defer release()
		r = r.WithContext(ctx)
	case r.Method == http.MethodGet && target.name == "":
		class = "list"
		m.Add("native.list_requests", 1)
		p.pending.listed(session.name + " " + r.URL.Path + "?" + selectorsOf(query))
	case r.Method == http.MethodGet:
		class = "get"
		m.Add("native.get_requests", 1)
	case r.Method == http.MethodPatch && target.resource == "widgets" && target.name != "":
		class = "patch"
		m.Add("native.patch_requests", 1)
		// Only a JSON merge patch: a JSON Patch or an apply patch has semantics the check below does
		// not cover.
		if r.Header.Get("Content-Type") != "application/merge-patch+json" {
			refuse(writer, http.StatusUnsupportedMediaType, "only application/merge-patch+json is forwarded")
			m.Add("native.patch_status."+statusClass(writer.status), 1)
			return
		}
		patch, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			refuse(writer, http.StatusRequestEntityTooLarge, "patch too large")
			return
		}
		if err := gateway.ValidateNativeMergePatch(patch); err != nil {
			refuse(writer, http.StatusBadRequest, err.Error())
			m.Add("native.patch_status."+statusClass(writer.status), 1)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(patch))
		r.ContentLength = int64(len(patch))
	default:
		m.Add("native.refused_method", 1)
		refuse(w, http.StatusMethodNotAllowed, "not proxied")
		return
	}
	writer.add = func(n int64) { m.Add("native.bytes."+class, n) }
	defer func() {
		m.Add("native."+class+"_status."+statusClass(writer.status), 1)
	}()
	p.proxy.ServeHTTP(writer, r)
}

func isWatch(q url.Values) bool {
	v := q.Get("watch")
	return v == "1" || v == "true"
}

// selectorsOf keeps the parts of a collection query that select members, so a LIST and its WATCH
// pair up whatever else the connector adds.
func selectorsOf(q url.Values) string {
	return url.Values{"labelSelector": q["labelSelector"], "fieldSelector": q["fieldSelector"]}.Encode()
}
