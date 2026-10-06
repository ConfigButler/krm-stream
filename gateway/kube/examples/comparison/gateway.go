package comparison

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/ConfigButler/krm-stream/gateway"
)

// The gateway source: four stream routes, one per projection and sharing choice. Each has its own
// upstream Backend, Observer and counters, so one route's work never shows up in another's numbers.
//
//	/stream/full         krm-full/v1, one upstream watch per stream
//	/stream/spec         krm-spec/v1, one upstream watch per stream
//	/stream/full-shared  krm-full/v1 through gateway.NewSharedBackend: one upstream watch per scope
//	/stream/spec-shared  krm-spec/v1 through gateway.NewSharedBackend
//
// Every upstream watch here is opened as the HOST's identity. A production host serving the unshared
// routes would act as each caller instead (see docs/auth.md); it would cost the same watches.

// Route describes one gateway stream route.
type Route struct {
	Name       string             `json:"name"`
	Path       string             `json:"path"`
	Projection gateway.Projection `json:"projection"`
	Shared     bool               `json:"shared"`
}

// Routes are the four gateway routes, in display order.
var Routes = []Route{
	{Name: "full", Path: "/stream/full", Projection: gateway.ProjectionFull},
	{Name: "spec", Path: "/stream/spec", Projection: gateway.ProjectionSpec},
	{Name: "full-shared", Path: "/stream/full-shared", Projection: gateway.ProjectionFull, Shared: true},
	{Name: "spec-shared", Path: "/stream/spec-shared", Projection: gateway.ProjectionSpec, Shared: true},
}

// countingBackend counts the upstream watches a route opens and how many are open now. It wraps the
// route's own kube.Backend, below any SharedBackend, so it counts what reaches the API server from
// this route alone: unlike a cluster-wide API-server gauge, nothing else running against the same
// cluster can move it.
type countingBackend struct {
	inner   gateway.Backend
	metrics *Metrics
	route   string
}

func (b *countingBackend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error) {
	b.metrics.Add("gateway."+b.route+".upstream_watch_calls", 1)
	w, err := b.inner.Watch(ctx, scope)
	if err != nil {
		b.metrics.Add("gateway."+b.route+".upstream_watch_errors", 1)
		return nil, err
	}
	b.metrics.Gauge("gateway."+b.route+".upstream_watches_active", 1)
	return &countingWatcher{Watcher: w, done: func() { b.metrics.Gauge("gateway."+b.route+".upstream_watches_active", -1) }}, nil
}

type countingWatcher struct {
	gateway.Watcher
	once sync.Once
	done func()
}

func (w *countingWatcher) Stop() {
	w.Watcher.Stop()
	w.once.Do(w.done)
}

// principal is the gateway's view of a host session.
type principal struct{ name string }

// streamRoute builds one route's handler over its backend.
func (h *Host) streamRoute(route Route, backend gateway.Backend) http.Handler {
	m := h.metrics
	prefix := "gateway." + route.Name
	obs := observer(m, route.Name)
	if route.Shared {
		backend = gateway.NewSharedBackend(backend, gateway.SharedOptions{Observer: obs})
	}
	stream := gateway.Handler(gateway.Options{
		Principal: func(r *http.Request) (gateway.Principal, error) {
			m.Add(prefix+".session_checks", 1)
			session, err := h.sessionOf(r, "gateway")
			if err != nil {
				return nil, gateway.Unauthenticated("sign in to watch these objects")
			}
			return principal{name: session.name}, nil
		},
		Scopes: gateway.ScopePolicy{
			Targets: []string{""},
			Resources: []gateway.GroupResource{
				{Group: WidgetGroup, Resource: "widgets", Scope: gateway.ResourceScopeNamespaced},
				{Resource: "secrets", Scope: gateway.ResourceScopeNamespaced},
			},
		},
		StreamConfig: gateway.StreamConfig{
			Authorizer: gateway.AuthorizerFunc(func(ctx context.Context, p gateway.Principal, s gateway.Scope) error {
				// A timed recheck runs under ReauthorizationTimeout; an opening or snapshot-cycle check
				// runs on the request's own context, which this host gives no deadline.
				kind := "cycle"
				if _, ok := ctx.Deadline(); ok {
					kind = "timed"
				}
				m.Add(prefix+".authorizer_calls."+kind, 1)
				caller, _ := p.(principal)
				if caller.name != sessionViewer {
					m.Add(prefix+".authorizer_refusals", 1)
					return gateway.Forbidden("this session may not watch these objects")
				}
				if s.Namespace != h.namespace {
					m.Add(prefix+".authorizer_refusals", 1)
					return gateway.Forbidden("outside this host's namespace")
				}
				return nil
			}),
			Clients:                 func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return backend, nil },
			Projections:             gateway.StaticProjection(route.Projection),
			Observer:                obs,
			WriteTimeout:            5 * time.Second,
			ReauthorizationInterval: h.reauthorizeEvery,
			ReauthorizationTimeout:  5 * time.Second,
		},
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.Add(prefix+".http_requests", 1)
		ctx, release := h.connections.open(r.Context(), "gateway")
		defer release()
		m.Gauge(prefix+".sse_active", 1)
		defer m.Gauge(prefix+".sse_active", -1)
		writer := &countingWriter{ResponseWriter: w, add: func(n int64) { m.Add(prefix+".sse_bytes", n) }}
		stream.ServeHTTP(writer, r.WithContext(ctx))
	})
}
