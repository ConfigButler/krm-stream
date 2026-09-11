# Adopting krm-stream

This is the shortest safe route from an existing Go application to a browser KRM stream. The library
owns the read path. Your application owns identity, authorization policy, Kubernetes credentials, and
writes.

## 1. Declare scopes precisely

An empty namespace has two Kubernetes meanings: a cluster-scoped resource, or every namespace for a
namespaced resource. `ScopePolicy` makes the host choose explicitly.

| Resource kind | `GroupResource` | Caller namespace | Result |
|---|---|---|---|
| one namespace of ConfigMaps | `Scope: ResourceScopeNamespaced` | `app` | watches only `app` |
| all namespaces of ConfigMaps | `Scope: ResourceScopeNamespaced, AllowAllNamespaces: true` | empty | watches all namespaces |
| Namespaces | `Scope: ResourceScopeCluster` | empty | watches cluster-scoped objects |
| Namespaces with `namespace=app` | `Scope: ResourceScopeCluster` | `app` | refused |

Keep all-namespaces access rare. It changes the size, disclosure risk, and operating cost of a stream.
Use an `Authorizer` to pin a user to a namespace or target before any watch opens.

### Errors from the exported surface are `error`

`ScopeFromQuery` and `ScopePolicy.Validate` return `error`, and the concrete value is a
`*StreamError`. Reach the wire code with `errors.As`:

```go
var serr *gateway.StreamError
if errors.As(err, &serr) {
	log.Printf("refused with %s: %s", serr.Code, serr.Message)
}
```

### Targets with a path prefix

A `rest.Config.Host` may include a path prefix, such as a kcp workspace URL
(`https://kcp.example/clusters/root:org:ws`). The dynamic client preserves it when constructing API
requests. Use `kube.NewBackendForConfig(cfg)` to include the endpoint in upstream error diagnostics;
`kube.NewBackend(dynamicClient)` also works when a dynamic client already exists.

## 2. Mount the same-origin cookie endpoint

```go
import (
    "net/http"

    "github.com/ConfigButler/krm-stream/gateway"
    "github.com/ConfigButler/krm-stream/gateway/kube"
)

func mount(mux *http.ServeMux, dynamicClientFor func(*User) dynamic.Interface) {
    mux.Handle("/resource-stream/v1", gateway.Handler(gateway.Options{
        // Your session cookie -> your application user. krm-stream never sees a token.
        Principal: func(r *http.Request) (gateway.Principal, error) {
            return userFromSession(r)
        },
        // Deny before opening a watch; also rechecked on every recovery cycle.
        Authorizer: gateway.AuthorizerFunc(func(ctx context.Context, p gateway.Principal, s gateway.Scope) error {
            user := p.(*User)
            if s.Target != user.Target || s.Namespace != user.Namespace {
                return gateway.Forbidden("scope is not available to this user")
            }
            return nil
        }),
        // Build a dynamic client acting as this user. Kubernetes RBAC remains the boundary.
        Clients: func(_ context.Context, _ string, p gateway.Principal) (gateway.Backend, error) {
            return kube.NewBackend(dynamicClientFor(p.(*User))), nil
        },
        Scopes: gateway.ScopePolicy{
            Targets: []string{"production"},
            Resources: []gateway.GroupResource{
                {Group: "", Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced},
                {Group: "apps", Resource: "deployments", Scope: gateway.ResourceScopeNamespaced},
            },
        },
        Projection: gateway.ProjectionFull,
    }))
}
```

The browser uses `connectManagedResourceStream` for this route. Fetch sends its same-origin session
cookie and the managed connection provides bounded recovery after network failures and sequence gaps.

## 3. Browser client

```ts
import {
  defaultPolicy,
  LiveResourceStore,
  connectManagedResourceStream,
  resourceStreamURL,
  withOpenAPIKeyedLists,
} from "@configbutler/krm-stream";

const store = new LiveResourceStore();
const url = resourceStreamURL("/resource-stream/v1", {
  target: "production",
  version: "v1",
  resource: "configmaps",
  namespace: "app",
  projection: "krm-full/v1",
});

const connection = connectManagedResourceStream(url, store, {
  onStateChange: state => renderConnection(state.status), // gaps recover with a fresh snapshot
});
store.subscribe(() => render(store));
```

For a bearer-token client, use `connectManagedResourceStream(url, store, { headers: { Authorization: ... } })`.
That is useful for a non-browser client or an intentionally token-bearing browser application; the
same-origin cookie route is the safer browser default. The host must enforce HTTPS for bearer-token
requests, including the resolved destination of relative URLs and any redirects. Validate the
trusted endpoint before supplying credentials and enforce the same policy in a custom fetch wrapper.
The stream connectors delegate transport to fetch; they do not enforce a credential transport policy.

For Deployment or CRD editing, a host may opt into OpenAPI-declared associative-list merging without
exposing schemas to the browser:

```ts
const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, deploymentSchema));
```

`deploymentSchema` is the structural schema for that exact GroupVersionKind. Only lists marked
`x-kubernetes-list-type: map` with `x-kubernetes-list-map-keys` are merged by key; every other list
stays safely atomic.

A quiet stream can still hold an older write version. See
[why a quiet stream can reject a save](saving.md#why-a-quiet-stream-can-still-reject-a-save)
for the illustrated flow and the host's recovery responsibilities.

## 4. Share watches only with Kubernetes-backed authorization

`SharedBackend` saves upstream watches but runs as one service identity. Pair it with
`kube.SubjectAccessReviewAuthorizer` so Kubernetes still decides whether each caller may list and watch the scope.

```go
shared := gateway.NewSharedBackendWithOptions(serviceAccountBackend, gateway.SharedOptions{
    QueueDepth: 512,
    Observer: metrics,
})

options.Authorizer = kube.SubjectAccessReviewAuthorizer(clientset, subjectFromUser)
options.Clients = func(context.Context, string, gateway.Principal) (gateway.Backend, error) { return shared, nil }
```

Read [auth.md](auth.md) before using this configuration. The shared-cache authorizer is a security
boundary; a per-user backend keeps Kubernetes RBAC as the direct boundary by construction.

Next: [saving.md](saving.md) for the host-owned write path and [operations.md](operations.md) for
stream monitoring and limits.
