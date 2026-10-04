# Operating krm-stream

The gateway reports low-cardinality lifecycle signals through `Observer`, so hosts can count them
without inspecting resources:

```go
metrics := gateway.ObserverFunc(func(o gateway.Observation) {
    streamsTotal.WithLabelValues(string(o.Kind), string(o.Projection), string(o.EventType)).Inc()
})
```

Do not block in `Observe`; it runs on the stream or shared-watch goroutine. Never label a metric with
an object name, UID, principal, patch contents, scope value or error message.

Error text goes to `Options.Diagnostics` instead, because it can name internal addresses and URLs; the
browser receives only the protocol code and a message chosen for it. Redact before logging.

## Signals to alert on

| Signal | Meaning | First response |
|---|---|---|
| `http_transport_rejected` | bounded HTTP serving lacks required writer capabilities; no logical stream opened | test the mounted middleware and provide flush/deadline support; aborted requests may reconnect |
| `consumer_resync` rising | upstream continuity was lost or a shared subscriber fell behind | correlate with API-server errors, shared overflows, and deployments |
| `shared_overflow` | one subscriber exceeded `SharedOptions.QueueDepth` | increase only after checking browser stalls and event rate; resnapshot is intentional |
| `terminal_error` | logical-stream failure, observed before attempting its terminal frame; delivery can fail | alert by low-cardinality error code; browsers must not retry terminal errors |
| `retryable_error` rising | streams ending with a non-terminal error, usually `UPSTREAM_UNAVAILABLE` | check API-server health and priority-and-fairness rejections; browsers reconnect on their own backoff |
| `event_suppressed` ratio | `krm-spec/v1` is removing expected churn | a sharp drop may mean callers selected `krm-full/v1` or a projection changed |
| stream count / snapshot duration | connection pressure or oversized scopes | narrow namespaces/selectors; avoid accidental all-namespaces watches |
| unorderable `resourceVersion` terminal errors | an unsupported or aggregated API does not meet strict ordering | use `OrderingLenient` only after accepting the reduced monotonicity guarantee |

## Troubleshooting upstream errors

`gateway/kube` maps API-server answers to protocol codes:

| API server | Browser receives |
|---|---|
| 403 | `FORBIDDEN`, with Kubernetes' message |
| 401 | `UNAUTHENTICATED` |
| 404 | `SCOPE_INVALID` |
| 429, 5xx, unreachable | `UPSTREAM_UNAVAILABLE`, with `Retry-After` as `retryAfterMs`; the connection closes and the client retries |

- **Streams stuck in `syncing` without errors.** client-go retries a 429 or 5xx that carries
  `Retry-After` up to ten times before the gateway sees it, and the browser waits with heartbeats
  meanwhile. Look at API-server load.
- **`retryable_error` from watches that end early.** A watch that ends before its snapshot completes,
  or within a second of it, is recovered once on the same connection; a second in a row closes the
  connection with `UPSTREAM_UNAVAILABLE`. Look for an upstream that ends every watch.
- **One shared scope slow to open.** It holds up only the streams waiting for that scope, and each
  ends with its request. A client-go watch has no response-header timeout of its own, so look at that
  scope's API server.
- **A shared scope refused with `retryAfterMs` while others work.** `SharedBackend` backs off a scope
  whose upstream will not open, so its subscribers' reconnects do not stampede.

## Runtime controls

| Control | Default | Use |
|---|---:|---|
| `gateway.Options.WriteTimeout` | 0 (no deadline) | set a positive per-operation write-plus-flush budget for bounded HTTP delivery; required with `ReauthorizationInterval` |
| `gateway.Options.ReauthorizationInterval` | 0 (cycle checks only) | recheck each subscriber on quiet streams; see the [revocation budget](auth.md#revocation-budget) |
| `gateway.Options.ReauthorizationTimeout` | 10 seconds | bound each periodic check's callbacks once it holds the delivery gate |
| `gateway.Options.HeartbeatInterval` | 20 seconds | set below the shortest proxy idle timeout |
| `gateway.SharedOptions.QueueDepth` | 256 live events | tune after measuring; it bounds memory per slow subscriber |
| `ScopePolicy.AllowLabelSelector` | false | enable only for an endpoint that deliberately supports caller narrowing |
| `GroupResource.AllowAllNamespaces` | false | make all-namespaces access an explicit reviewable policy decision |
| `Gateway.Ordering` | strict | keep strict on supported Kubernetes; use lenient only for known aggregated APIs |
| `gateway.Options.Diagnostics` | nil (discarded) | receive the raw error behind each error event; redact before logging |

The gateway sets no snapshot object or byte limit: what is safe depends on the product. Measure
snapshot size and duration per allowed scope before opening broad all-namespaces endpoints.

## Counting lifetimes

`stream_opened`/`stream_closed` count `StreamProjection` entry/return, including authorization
failure, but exclude HTTP identity/scope refusals before entry. `shared_subscription_opened` and
`shared_subscription_closed` count active attachments, including warm-cache joins. Overflow, scope
death or leave ends an attachment once; repeated `Stop` does not count again. A resnapshot can
replace an attachment on one logical stream without another API watch.

Callbacks are synchronous and may run concurrently, including under shared locks. Return promptly;
do not panic or reenter the gateway. Update gauges synchronously rather than through a lossy exporter
queue. Opens precede matching closes, with no global order across lifetimes. Ignore unknown kinds.
See the tested [counter mapping](../gateway/kube/examples/sharedstream/metrics.go).

These gauges do not count HTTP requests still resolving identity, or upstream watch handles. Measure
physical API-server WATCH requests separately; the shared-host fixture uses
`apiserver_longrunning_requests` filtered to the resource and `verb="WATCH"`.

## Bounded HTTP delivery

Set `WriteTimeout` explicitly. It is required with timed reauthorization: `Handler` panics at
construction without it, and `ServeStream` panics before writing. Each header, event or heartbeat
write and its flush share one deadline, cleared after success, so a quiet stream outlives many
timeout periods. A failed write ends its stream. Flush success does not acknowledge browser receipt.

The library owns write deadlines while serving and clears them at exit; it cannot restore a host's
previous deadline. A whole-response `http.Server.WriteTimeout` is no substitute on a long-lived
stream. The bound covers I/O only, not backend opening, gate waiting, encoding or host callbacks. A
generic `Stream` sink is the host's to bound.

With a positive bound, an unsupported writer aborts before streaming and emits
`http_transport_rejected`. Test your mounted middleware with the
[capability-check recipe](../gateway/kube/examples/sharedstream/README.md#middleware-capability-test).
HTTP/1.1, TLS HTTP/2 (including two streams on one connection), and transparent and opaque wrappers
are tested; other middleware and proxy combinations need host tests.

For authorization bursts and the limits of the 200-subscriber profile, see the
[shared-host capacity guide](../gateway/kube/examples/sharedstream/README.md#verification-and-capacity).
