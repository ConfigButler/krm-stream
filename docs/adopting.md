# Adopting krm-stream

This is the recommended route from an existing Go application to a live KRM view in the browser. The
library owns the read path. Your application owns identity, authorization policy, Kubernetes
credentials and writes.

## 1. Mount the stream endpoint

Each browser stream acts as its caller: the backend is built from that user's credential, so
Kubernetes RBAC stays the boundary. The session cookie identifies the caller, and the request ends
when the session does.

```go
import (
    "context"
    "log/slog"
    "net/http"
    "time"

    "k8s.io/client-go/rest"

    "github.com/ConfigButler/krm-stream/gateway"
    "github.com/ConfigButler/krm-stream/gateway/kube"
)

func mount(mux *http.ServeMux, restConfigFor func(*User) *rest.Config) {
    stream := gateway.Handler(gateway.Options{
        // Your session cookie -> your application user. krm-stream never sees a token.
        Principal: func(r *http.Request) (gateway.Principal, error) {
            return userFromSession(r)
        },
        // Deny before opening a watch; rechecked on every snapshot cycle.
        Authorizer: gateway.AuthorizerFunc(func(ctx context.Context, p gateway.Principal, s gateway.Scope) error {
            user := p.(*User)
            if s.Target != user.Target || s.Namespace != user.Namespace {
                return gateway.Forbidden("scope is not available to this user")
            }
            return nil
        }),
        // A backend acting as this user.
        Clients: func(_ context.Context, _ string, p gateway.Principal) (gateway.Backend, error) {
            return kube.NewBackendForConfig(restConfigFor(p.(*User)))
        },
        Scopes: gateway.ScopePolicy{
            Targets: []string{"production"},
            Resources: []gateway.GroupResource{
                {Group: "", Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced},
                {Group: "apps", Resource: "deployments", Scope: gateway.ResourceScopeNamespaced},
            },
        },
        Projection:   gateway.ProjectionFull,
        WriteTimeout: 10 * time.Second, // bound each write to a browser that stops reading
        Diagnostics: func(d gateway.Diagnostic) {
            slog.Warn("stream error", "code", d.Code, "resource", d.Scope.Resource, "err", redact(d.Err))
        },
    })
    mux.Handle("/resource-stream/v1", endWithSession(stream))
}

// endWithSession ends a stream when the caller's session expires.
func endWithSession(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if expiry, ok := sessionExpiry(r); ok {
            ctx, cancel := context.WithDeadline(r.Context(), expiry)
            defer cancel()
            r = r.WithContext(ctx)
        }
        next.ServeHTTP(w, r)
    })
}
```

`WriteTimeout` needs a response writer that supports flushing and write deadlines. Check your mounted
middleware with the
[capability test](../gateway/kube/examples/sharedstream/README.md#middleware-capability-test);
an unsupported writer aborts the request before streaming. The
[shared-host example](../gateway/kube/examples/sharedstream/handler.go) shows a complete session
resolver that uses the earlier of session and token expiry.

## 2. Declare scopes precisely

An empty namespace has two Kubernetes meanings: a cluster-scoped resource, or every namespace of a
namespaced resource. `ScopePolicy` makes the host choose explicitly.

| Resource kind | `GroupResource` | Caller namespace | Result |
|---|---|---|---|
| one namespace of ConfigMaps | `Scope: ResourceScopeNamespaced` | `app` | watches only `app` |
| all namespaces of ConfigMaps | `Scope: ResourceScopeNamespaced, AllowAllNamespaces: true` | empty | watches all namespaces |
| Namespaces | `Scope: ResourceScopeCluster` | empty | watches cluster-scoped objects |
| Namespaces with `namespace=app` | `Scope: ResourceScopeCluster` | `app` | refused |

Keep all-namespaces access rare: it changes the size, disclosure risk and operating cost of a stream.
Use the `Authorizer` to pin a user to a namespace or target before any watch opens.

## 3. Connect the browser

```ts
import {
  LiveResourceStore,
  applyStreamEvent,
  connectResourceStream,
  resourceStreamURL,
} from "@configbutler/krm-stream";

const store = new LiveResourceStore();
const url = resourceStreamURL("/resource-stream/v1", {
  target: "production",
  version: "v1",
  resource: "configmaps",
  namespace: "app",
  projection: "krm-full/v1",
});

const connection = connectResourceStream(url, event => applyStreamEvent(store, event));
renderConnection(connection.state.status);
connection.subscribe(state => renderConnection(state.status)); // gaps recover with a fresh snapshot
connection.closed.catch(reportApplicationError); // the callback threw, and the stream stopped
store.subscribe(() => render(store));
```

Fetch sends the same-origin session cookie, and the connection recovers on its own after network
failures and sequence gaps.

Saving is the host's: see [saving](saving.md). A quiet stream can still hold an older write version;
see [why a quiet stream can reject a save](saving.md#why-a-quiet-stream-can-still-reject-a-save).

## 4. Optional: recheck quiet streams on a timer

Every snapshot cycle reauthorizes, but a quiet stream can go a long time without one. To bound how
long a revoked caller keeps it, opt into timed checks, and make the `Authorizer` check that the
caller's session is still valid, since the principal is the one captured when the stream opened:

```go
options.ReauthorizationInterval = 30 * time.Second
options.ReauthorizationTimeout = 5 * time.Second
options.WriteTimeout = 10 * time.Second // required with timed checks; Handler panics without it
```

Plan with the [revocation budget](auth.md#revocation-budget).

## 5. Optional: share watches, with Kubernetes-backed authorization

`SharedBackend` opens one upstream watch per scope for every caller, as one service identity. Your
`Authorizer` is then the only check between a caller and the cached objects, so pair it with
`kube.SubjectAccessReviewAuthorizer`, which asks Kubernetes whether each caller may list and watch
the scope:

```go
shared := gateway.NewSharedBackendWithOptions(serviceAccountBackend, gateway.SharedOptions{
    QueueDepth: 512,
    Observer:   metrics,
})

options.Authorizer = kube.SubjectAccessReviewAuthorizer(clientset, subjectFromUser)
options.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return shared, nil }
```

Read [shared-watch authorization](auth.md#shared-watch-authorization) first. The
[shared ConfigMap host](../gateway/kube/examples/sharedstream/README.md) is a tested composition with
session expiry, timed checks and a write timeout.

Next: [saving](saving.md) for the host-owned write path and [operations](operations.md) for
monitoring and limits.

## Reference

### Errors and diagnostics

A `StreamError` you return keeps its `Message`; its `Cause` never reaches the wire. Any other error
reaches the browser as `INTERNAL` with the message `internal error`. `Options.Diagnostics` receives
the raw error with its principal and scope; log what your policy allows.

`Principal` may return `gateway.Unauthenticated` when signing in again will help, or
`gateway.UpstreamUnavailable` when your session store is down. Any other `Principal` error is sent as
`UNAUTHENTICATED` "not authenticated".

`ScopeFromQuery` and `ScopePolicy.Validate` return `error` whose concrete value is a `*StreamError`;
reach its code with `errors.As`.

### Delegating resource admission to Kubernetes

An endpoint that should expose whatever Kubernetes lets each user read, as a `/k8s` proxy does, can
replace the resource list with `AnyResource`:

```go
Scopes: gateway.ScopePolicy{
	Targets:     []string{""},
	AnyResource: true, // admission is left to the upstream's own authorization
},
```

Any resource the scope syntax accepts then passes, with or without a namespace, and the API server
decides. The target allowlist, `AllowLabelSelector`, the refusal of API-server addresses and the
`Authorizer` still apply.

`AnyResource` is safe only when `Clients` returns a backend built from the caller's own credential.
The library cannot check that: with a service account's client or a hidden `SharedBackend`, every
resource that identity can read is open to everyone your `Authorizer` admits. Prove the binding in
your own tests.

### Targets with a path prefix

A `rest.Config.Host` may include a path prefix, such as a kcp workspace URL
(`https://kcp.example/clusters/root:org:ws`); the dynamic client preserves it.
`kube.NewBackendForConfig(cfg)` includes the endpoint in upstream errors, which reach
`Options.Diagnostics` and never the browser. To use `kube.NewBackend(dynamicClient)` instead, build
the client on `kube.HTTPClientFor(cfg)` (`dynamic.NewForConfigAndClient(cfg, httpClient)`), which
refuses redirects so a user's token is not carried along.

### Bearer-token clients

`connectResourceStream(url, consume, { headers: { Authorization: ... } })` suits a non-browser
client or an intentionally token-bearing browser application; the same-origin cookie route is the
safer browser default. The connector delegates transport to fetch and enforces no credential policy,
so the host must require HTTPS for the resolved destination, including redirects, before supplying
credentials.

### Keyed lists for Deployment and CRD editing

A host may opt into OpenAPI-declared associative-list merging without exposing schemas to the browser:

```ts
const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, deploymentSchema));
```

`deploymentSchema` is the structural schema for that exact GroupVersionKind. Only lists marked
`x-kubernetes-list-type: map` with `x-kubernetes-list-map-keys` are merged by key; every other list
stays atomic.
