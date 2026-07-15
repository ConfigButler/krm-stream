# Why a gateway

Why krm-stream puts a server between the browser and the Kubernetes API, rather than letting the
browser watch the API server itself.

## The mechanism it builds on

Kubernetes has an efficient change feed. A `GET` with `?watch=1` streams `ADDED`, `MODIFIED` and
`DELETED` events for a resource, with `resourceVersion` as the position in the stream, bookmarks to
keep that position cheap, and `410 Gone` when the position has aged out of the server's cache. It is
documented under
[efficient detection of changes](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes),
and it is what the gateway consumes upstream. Nothing here replaces it.

## Why the browser cannot use it directly

Not for transport reasons. A watch is an ordinary chunked HTTP response carrying newline-delimited
JSON. It is not a protocol upgrade; upgrades are what `exec`, `attach` and `port-forward` need, and a
watch is not one of them. The obstacles are around the stream rather than in it.

**It needs a cluster credential.** A watch means presenting a bearer token or a client certificate
that Kubernetes RBAC recognises. Shipping either to a browser gives every tab, and anything running
in it, an identity in your cluster. There is no restriction you can attach in the browser that the
browser cannot also remove.

**`EventSource` cannot read it.** A watch is newline-delimited JSON, not SSE framing, and
`EventSource` cannot set an `Authorization` header. Consuming a watch in a browser means `fetch`, a
`ReadableStream`, and your own reconnection and resume logic, which returns you to the credential
problem with more code around it.

**The API server serves no CORS.** Reaching it cross-origin requires `--cors-allowed-origins` set
cluster-wide on the API server, for your web application. That is the concession
`@hawtio/kubernetes-api` required (see [alternatives](alternatives.md)), and most operators will not
make it.

**The browser would see whole objects.** A watch returns everything: `Secret` data, `managedFields`,
`status`, fields belonging to other tenants of the same namespace. Withholding those has to happen
somewhere the user does not control, which means the server.

The gateway is that server. It holds the credential, applies the projection and its redactions,
enforces the scope, and re-frames the result as SSE, which the browser reads natively with no bundler
and no reconnection logic in your application.

## Why watches are shared

A watch is not free upstream. Each one is a connection and a registered watcher on the API server,
delivering every event in its scope. Ten tabs on the same namespace, watching directly, are ten
watches, ten snapshots and ten copies of the same object graph. Reopen a floor of laptops at once and
that reconnect storm hits the API server multiplied by the number of tabs.

[`gateway.SharedBackend`](../gateway/shared.go) opens one upstream watch per scope rather than per
tab, and serves every subscriber from its cache. A tab joining a scope that is already open gets its
`reset`…`synced` snapshot from that warm cache without reaching the API server at all.

Sharing is opt-in, because the trade is real. A shared watch can be opened only once, so it runs as
one identity: your service account. Without sharing, the client acts as the caller and Kubernetes RBAC
enforces the boundary, so no bug in this library can hand someone an object they may not see. With
sharing, your `Authorizer` is the only thing between a caller and the cache.

You can have both. Pair `SharedBackend` with [`kube.SSARAuthorizer`](../gateway/kube/authz.go), which
asks the API server through a SubjectAccessReview whether this user may list and watch this resource
here, before serving them from the shared cache. Kubernetes decides again, per user, per snapshot
cycle, at the cost of one round-trip. Read [auth.md](auth.md) before wiring it.
