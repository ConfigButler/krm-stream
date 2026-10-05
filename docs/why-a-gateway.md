# Watching resources

krm-stream starts with Kubernetes watches and delivers a defined live resource view to a browser.
Use it for lists and status pages, then add the [editor](client-state-model.md) when a page needs
local drafts. The host owns authentication, authorization policy, credentials and writes.

## Choose a scope and a view

Scope selects the target, resource kind, namespace, optional name and allowed label selector.
Projection selects what is delivered. Suppression decides whether a change to that view needs a
new object event. These are separate decisions.

The host maps target names to configured clusters and authorizes the scope and projection. The
browser can request a named view; it cannot supply arbitrary field rules or an API-server URL.
There is no general gateway field selector or built-in status-only projection.

| Projection | Fields delivered | Secret values | Updates suppressed |
|---|---|---|---|
| `krm-full/v1` | Includes status | Withheld | Bookkeeping-only |
| `krm-spec/v1` | Omits status | Withheld | Bookkeeping-only and status-only |
| `krm-raw/v1` | Includes status | Included when host policy permits | Bookkeeping-only |

Every view strips `metadata.managedFields` and the last-applied-configuration annotation. Raw is a
named projection, not native passthrough. Full/spec redaction covers core Kubernetes Secret values,
not arbitrary sensitive content in ConfigMaps or custom resources. Withheld key paths and change
revisions are disclosed in `redacted`; the object contains no mask to accidentally save back.

For a host that permits these requests:

```text
/resource-stream/v1?target=production&version=v1&resource=configmaps&namespace=app&projection=krm-full/v1
/resource-stream/v1?target=production&group=apps&version=v1&resource=deployments&namespace=app&projection=krm-spec/v1
/resource-stream/v1?target=production&version=v1&resource=pods&namespace=app&labelSelector=app%3Dhello&projection=krm-full/v1
```

Enable `ScopePolicy.AllowLabelSelector` deliberately before accepting the last request. Use
`resourceStreamURL` to encode queries in application code; see [adoption](adopting.md).

## What suppression saves

The gateway compares projected content excluding `metadata.resourceVersion`, plus redaction records,
with what it last delivered for that UID. Unchanged views produce no object event. Secret rotations
still produce updates because their redaction revisions change. Every complete snapshot includes all
members, including unchanged objects.

This reduces downstream bytes and event processing. Fewer store notifications and renders depend
on how the application consumes events; suppression does not eliminate the gateway's upstream work.
The stream represents current state and may coalesce intermediate changes; use an audit system when
every transition matters.

A quiet view can hold an older write version even while its displayed fields are correct. That
tradeoff matters when adding editing: see [quiet streams and saving](saving.md#why-a-quiet-stream-can-still-reject-a-save).

## Snapshot and recovery

Each connection begins with `reset`, zero or more resource upserts, then `synced`. Live upserts replace
complete projected objects. `reset` retains old state while the snapshot arrives; only `synced` prunes
members that were not resent. An interrupted snapshot cannot prove absence. UID distinguishes a
recreated resource from an earlier object with the same name.

The fetch connector exposes `connecting`, `syncing`, `live`, `retrying`, `closed`, `terminal` and
`exhausted`. Apply state events synchronously before rendering completion. Retryable failures recover
within the connector's bounded budget; 401/403 and terminal stream errors stop the connection.
Dispose subscriptions and close the handle when their owner leaves. See the
[client lifecycle reference](../packages/krm-stream/README.md#connections).

A new browser connection receives a fresh snapshot. SSE sequence numbers and object versions are not
browser resume tokens. Proposed gateway upstream continuation can reduce resnapshots without changing
that browser contract; see [proposal 0006](proposals/0006-stream-and-save-implementation-plan.md#4-measured-upstream-continuation).

## Optional watch sharing

Without sharing, each stream uses its own backend watch. `SharedBackend` can keep one upstream watch
per matching scope in that backend and serve subscribers from its cache. Each subscriber still needs
its own snapshot, delivery and browser reconciliation. Different scopes or host processes do not
become one watch automatically.

A shared watch uses one service identity. The host authorizes each subscriber before serving cached
objects, checks its disclosure policy, and configures timed checks when quiet-stream revocation must
be bounded. Pair sharing with `kube.SubjectAccessReviewAuthorizer` for Kubernetes permission checks.
See [shared-watch authorization](auth.md#shared-watch-authorization) and [operations](operations.md).
Sharing reduces duplicate resource consumption; it does not supply a rate limiter or admission policy.

## Native watch direction and SSE compatibility

The preferred direction is a fetch connector that consumes native Kubernetes watch JSON through a
host proxy and reuses the frontend lifecycle and state-event input. An application with native API
access should not need to wrap those frames in SSE to use the resource store. Native transport keeps
upstream fields and does not provide the gateway's projection, redaction, suppression or sharing.

Keep the Go gateway and SSE as a supported compatibility path for consumers choosing that protocol,
and as the existing delivery path for projected views. SSE is framing; fetch and `EventSource` are
client mechanisms. Both native JSON and SSE can carry errors. The current fetch-based SSE connector
already handles HTTP refusal, terminal in-stream errors, cancellation and bounded retries. Native
`EventSource` has different header and retry limitations and cannot parse Kubernetes watch JSON.

A same-origin, cookie-authenticated host proxy can provide native access while retaining cluster
credentials server-side. The [native connector request](field-reports/third-our-identity.md#native-watch-connector)
is not implemented yet. Until its recovery and editing contracts pass acceptance, use the gateway
connector described in [adoption](adopting.md). Applications must choose a source explicitly and never fall back to native access
after a projected stream is refused.

Use [adoption](adopting.md) for today's supported wiring and [saving](saving.md) when adding writes.
