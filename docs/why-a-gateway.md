# Why a gateway

The gateway embeds Kubernetes reads in a browser application while keeping credentials and policy
on the host. It also gives the browser a snapshot boundary and a stable event vocabulary.

## From Kubernetes watch to browser stream

A Kubernetes watch is a chunked HTTP response containing newline-delimited JSON. It carries object
updates, bookmarks and history-expiry errors. Native `EventSource` expects SSE framing and cannot set
an authorization header, so it cannot consume that response directly.

Direct browser access would also require cluster credentials and appropriate API-server CORS
configuration. Raw watch objects include fields such as Secret values and managed fields. The host
must decide which scopes and fields it can disclose before sending them to the browser.

The gateway uses a host-supplied Kubernetes client, applies the selected projection and emits
`reset` … `synced` snapshots followed by live updates. The managed connector handles bounded browser
reconnection; a new connection receives a fresh snapshot. See the
[architecture diagram](../README.md#how-it-fits) and [protocol](../spec/v1.md).

## Optional watch sharing

Without sharing, each stream uses its own backend watch. `SharedBackend` can instead keep one
upstream watch per scope and serve each subscriber from its cache. A joining subscriber still
receives a complete projected snapshot, so sharing saves upstream work without eliminating browser
transfer or reconciliation costs.

A shared watch uses one service identity. Pair it with `kube.SubjectAccessReviewAuthorizer` to check
each subscriber's Kubernetes permissions before serving the cache, and configure timed checks when
quiet-stream revocation must be bounded. See [authorization](auth.md) and [operations](operations.md).
