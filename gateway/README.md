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
| `Scopes` | Allowlist targets, and allowlist group/resource combinations or delegate them to Kubernetes. |

The zero `ScopePolicy` denies every request. The gateway never accepts an API-server URL or a
credential from a browser request.

```go
handler := gateway.Handler(gateway.Options{
	Principal:  principalFromSession,
	Authorizer: authorizeScope,
	Clients: func(_ context.Context, target string, p gateway.Principal) (gateway.Backend, error) {
		return kube.NewBackendForConfig(restConfigFor(target, p))
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
a new cycle on the same SSE connection. Any other non-terminal error, such as `UPSTREAM_UNAVAILABLE`,
is sent with its `retryAfterMs` and then closes the connection: the client owns that retry. Terminal
errors are the final event and close the connection.

An unexpected error reaches the browser as `INTERNAL` with the message `internal error`. Its text,
and the `Cause` of any `StreamError`, go only to `Options.Diagnostics`, so the host decides what to
log and what to redact. `gateway/kube` maps API-server failures to protocol codes: 403 to
`FORBIDDEN` with Kubernetes' own message, 401 to `UNAUTHENTICATED`, 404 to `SCOPE_INVALID`, and 429,
5xx or an unreachable server to `UPSTREAM_UNAVAILABLE` (with `Retry-After` as `retryAfterMs`).
`SharedBackend` backs off a scope whose upstream will not open, so its subscribers' reconnects do
not add up to a stampede.

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
watch uses one identity. Pair it with `kube.SubjectAccessReviewAuthorizer` when Kubernetes should continue making
per-caller access decisions.

`SharedOptions.QueueDepth` bounds live events per slow subscriber. Overflow triggers a resnapshot;
it does not permit unbounded memory growth or silently drop events.

Each scope opens its upstream watch independently: a scope that is slow to open delays only the
callers waiting for that scope. A caller waiting for an opening leaves when its own request context
ends, and the last one to leave cancels the opening. The upstream `Backend` must honour context
cancellation for that to stop its work; a result it returns after everyone left is stopped and
discarded, and cannot affect a newer opening, the cache or the backoff.

## Operations

`Options.HeartbeatInterval` controls SSE keepalives. `Observer` provides low-cardinality lifecycle
signals for stream opens, cycles, emitted/suppressed events, resyncs, overflows, and terminal errors.
Observers run on stream paths, so they must return promptly.

See [`docs/operations.md`](../docs/operations.md) for suggested metrics and alerts, and
[`docs/adopting.md`](../docs/adopting.md) for full host wiring examples.

For shared streams, set `Options.ReauthorizationInterval` (for example, 30 seconds) and
`Options.ReauthorizationTimeout` (for example, 5 seconds). Each subscriber is rechecked independently,
including during quiet periods; denial or timeout stops only that subscriber. Zero interval keeps
cycle-only checks. See [authorization](../docs/auth.md) for callback contracts and capacity planning.
Use `kube.SubjectAccessReviewAuthorizer` for Kubernetes-backed subscriber authorization.

`Options.WriteTimeout` bounds each HTTP header/frame/comment write plus flush. Zero installs no
library deadline; the [shared-host example](kube/examples/sharedstream/README.md) chooses five seconds.
An unsupported writer aborts with `http_transport_rejected` before a logical stream opens. Use
`CheckHTTPStreaming` in a mounted middleware test; it clears the write deadline without writing.

`stream_closed` balances logical stream opens; `shared_subscription_opened` and
`shared_subscription_closed` describe active attachments, not upstream watch counts. The observer
is synchronous and concurrent: update counters promptly and do not reenter the gateway.
See [operations](../docs/operations.md) for exact counting and timeout contracts.
