# Go gateway

The `gateway` module converts a Kubernetes-style `Backend` watch into the KRM resource-stream
protocol. It is transport-neutral until `Handler` or `ServeStream` adds SSE framing. It has no
Kubernetes client dependency; [`gateway/kube`](kube/) provides the `client-go` backend and the
SubjectAccessReview authorizer.

```go
handler := gateway.Handler(gateway.Options{
	Principal: principalFromSession, // who is calling
	Scopes: gateway.ScopePolicy{ // the allowlist; the zero value denies everything
		Targets: []string{"production"},
		Resources: []gateway.GroupResource{
			{Group: "", Resource: "configmaps", Scope: gateway.ResourceScopeNamespaced},
		},
	},
	StreamConfig: gateway.StreamConfig{
		Authorizer: authorizeScope, // may they open this scope?
		Clients: func(_ context.Context, target string, p gateway.Principal) (gateway.Backend, error) {
			return kube.NewBackendForConfig(restConfigFor(target, p)) // acting as the caller
		},
		Projections:  gateway.StaticProjection(gateway.ProjectionFull),
		WriteTimeout: 10 * time.Second,
	},
})
```

`Handler` panics at construction on a missing seam or an unsafe option. The gateway never accepts an
API-server URL or a credential from a browser request.

`StreamConfig` holds the settings a stream needs however it is served, with one name each. `Options`
adds the HTTP-only `Principal` and `Scopes`; a host that routes and authorizes its own requests embeds
the same `StreamConfig` in a `Gateway` and calls `ServeStream`, or `Stream` with its own `Sink`. Both
take the projection name the caller requested; empty asks the `Projections` policy for its default.
Nil `Projections` grants only `ProjectionFull`.

## Stream behavior

Every snapshot cycle emits `reset`, zero or more `added` events, then `synced`; live updates are
complete-object replacements. Lost upstream continuity emits `RESYNC_REQUIRED` and starts a new cycle
on the same connection. Any other error ends the connection: a non-terminal one such as
`UPSTREAM_UNAVAILABLE` carries `retryAfterMs` and the client retries; a terminal one is the final
event. Unexpected error text goes only to `StreamConfig.Diagnostics`.

The gateway absorbs bookmarks, relists, 410 responses, partial metadata objects and ambiguous
deletion tombstones. `StreamConfig.Ordering` defaults to strict decimal `resourceVersion` ordering for
Kubernetes 1.35+, preserving per-object monotonicity within a cycle; use `OrderingLenient` only for an
upstream whose versions cannot be ordered. The normative details are in [`spec/v1.md`](../spec/v1.md).

## Projections

| Projection | Purpose |
|---|---|
| `krm-raw/v1` | Full upstream object for a host that has already made its own disclosure decision. |
| `krm-full/v1` | Removes metadata noise and redacts Secret values. |
| `krm-spec/v1` | The full view without status-driven browser churn. |

The gateway never writes. `ValidateMergePatch` checks a browser's patch against the projection before
the host sends it; see [saving](../docs/saving.md).

## Shared backends

`SharedBackend` is opt-in: it shares one upstream watch per scope, opened as one identity, so your
`Authorizer` becomes the access boundary. Each scope opens independently, and a caller waiting for
an opening can leave with its request; the last to leave cancels it. A slow subscriber is
resnapshotted after `SharedOptions.QueueDepth` live events rather than buffered without bound. See
[shared-watch authorization](../docs/auth.md#shared-watch-authorization).

## Guides

| Guide | For |
|---|---|
| [Adopting](../docs/adopting.md) | The recommended host and browser wiring, start to finish. |
| [Authorization](../docs/auth.md) | Backend choice, session validity, timed checks and the revocation budget. |
| [Saving](../docs/saving.md) | The host-owned conditional write path. |
| [Operations](../docs/operations.md) | Runtime controls, observations, alerts and bounded HTTP delivery. |
