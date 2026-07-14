# Go gateway

The `gateway` module converts a Kubernetes-style `Backend` watch into the KRM resource-stream
protocol. It is transport-neutral until `Handler` or `ServeStream` adds SSE framing.

The package has no Kubernetes client dependency. Use [`gateway/kube`](kube/) when `client-go` is the
right upstream for the host application.

## Required host seams

`gateway.Handler` requires four deliberate choices:

| Option | Host responsibility |
|---|---|
| `Principal` | Resolve the HTTP request to an application principal. |
| `Authorizer` | Allow or deny the normalized scope before a watch opens. |
| `Clients` | Return a backend acting as the caller, or an explicitly shared backend. |
| `Scopes` | Allowlist target and Kubernetes group/resource combinations. |

The zero `ScopePolicy` denies every request. The gateway never accepts an API-server URL or a
credential from a browser request.

```go
handler := gateway.Handler(gateway.Options{
	Principal:  principalFromSession,
	Authorizer: authorizeScope,
	Clients: func(_ context.Context, target string, p gateway.Principal) (gateway.Backend, error) {
		return kube.NewBackend(dynamicClientFor(target, p)), nil
	},
	Scopes: gateway.ScopePolicy{
		Targets: []string{"production"},
		Resources: []gateway.GroupResource{
			{Group: "", Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced},
			{Group: "", Resource: "namespaces", Scope: gateway.ResourceScopeCluster},
		},
	},
	Projection: gateway.ProjectionFull,
})
```

## Scope policy

An empty namespace is not a wildcard by default.

| Resource declaration | Request namespace | Result |
|---|---|---|
| `ResourceScopeNamespaced` | non-empty | one namespace |
| `ResourceScopeNamespaced` with `AllowAllNamespaces: true` | empty | all namespaces |
| `ResourceScopeCluster` | empty | cluster-scoped resource |
| `ResourceScopeCluster` | non-empty | refused |

`ScopePolicy.AllowLabelSelector` is also false by default. Enabling selectors permits caller
narrowing only; a host should still constrain selector complexity at its HTTP boundary.

## Stream behavior

Every snapshot cycle emits `reset`, zero or more `added` events, then `synced`. Live updates are
complete-object replacements. A recoverable upstream discontinuity emits `RESYNC_REQUIRED` and starts
a new cycle on the same SSE connection. Terminal errors are the final event and close the connection.

The gateway absorbs Kubernetes-specific mechanics including bookmarks, relists, 410 responses,
partial metadata objects, and ambiguous deletion tombstones. The normative details are in
[`spec/v1.md`](../spec/v1.md).

`Gateway.Ordering` defaults to strict decimal `resourceVersion` ordering. It targets Kubernetes 1.35+
and preserves per-object monotonicity within a snapshot cycle. Use `OrderingLenient` only for an
upstream whose versions cannot be ordered and only after accepting that reduced guarantee.

## Projections and saves

The built-in projections are:

| Projection | Purpose |
|---|---|
| `krm-raw/v1` | Full upstream object for a host that has already made its own disclosure decision. |
| `krm-full/v1` | Removes metadata noise and redacts Secret values. |
| `krm-spec/v1` | The full view without status-driven browser churn. |

The gateway never writes. A host save handler should read the current object under the same identity
and projection decision, then call `ValidateMergePatch` before issuing a Kubernetes merge patch. See
[`docs/saving.md`](../docs/saving.md).

## Shared backends

`SharedBackend` multiplexes one upstream watch per normalized scope. It is opt-in because the shared
watch uses one identity. Pair it with `kube.SSARAuthorizer` when Kubernetes should continue making
per-caller access decisions.

`SharedOptions.QueueDepth` bounds live events per slow subscriber. Overflow triggers a resnapshot;
it does not permit unbounded memory growth or silently drop events.

## Operations

`Options.HeartbeatInterval` controls SSE keepalives. `Observer` provides low-cardinality lifecycle
signals for stream opens, cycles, emitted/suppressed events, resyncs, overflows, and terminal errors.
Observers run on stream paths, so they must return promptly.

See [`docs/operations.md`](../docs/operations.md) for suggested metrics and alerts, and
[`docs/adopting.md`](../docs/adopting.md) for full host wiring examples.
