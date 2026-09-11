# Operating krm-stream

Long-lived streams need a small runbook. The gateway exposes low-cardinality `Observer` callbacks so
hosts can increment Prometheus counters or emit structured logs without inspecting resources.

```go
metrics := gateway.ObserverFunc(func(o gateway.Observation) {
    streamsTotal.WithLabelValues(string(o.Kind), string(o.Projection), string(o.EventType)).Inc()
})
```

Do not block in `Observe`; it runs on the stream or shared-watch goroutine. Never label a metric with
object name, UID, principal, patch contents, or an error message.

## Signals to alert on

| Signal | Meaning | First response |
|---|---|---|
| `http_transport_rejected` | bounded HTTP serving lacks required writer capabilities; no logical stream opened | test the mounted middleware and provide flush/deadline support; aborted requests may reconnect |
| `consumer_resync` rising | upstream continuity was lost or a shared subscriber fell behind | correlate with API-server errors, shared overflows, and deployments |
| `shared_overflow` | one subscriber exceeded `SharedOptions.QueueDepth` | increase only after checking browser stalls and event rate; resnapshot is intentional |
| `terminal_error` | logical-stream failure, observed before attempting its terminal frame; delivery can fail | alert by low-cardinality error code; browsers must not retry terminal errors |
| `event_suppressed` ratio | `krm-spec/v1` is removing expected churn | a sharp drop may mean callers selected `krm-full/v1` or a projection changed |
| stream count / snapshot duration | connection pressure or oversized scopes | narrow namespaces/selectors; avoid accidental all-namespaces watches |
| unorderable `resourceVersion` terminal errors | an unsupported or aggregated API does not meet strict ordering | use `OrderingLenient` only after accepting the reduced monotonicity guarantee |

## Runtime controls

| Control | Default | Use |
|---|---:|---|
| `gateway.Options.WriteTimeout` | 0 (no deadline) | set a positive per-operation write-plus-flush budget for bounded HTTP delivery |
| `gateway.Options.HeartbeatInterval` | 20 seconds | set below the shortest proxy idle timeout |
| `gateway.SharedOptions.QueueDepth` | 256 live events | tune after measuring; it bounds memory per slow subscriber |
| `ScopePolicy.AllowLabelSelector` | false | enable only for an endpoint that deliberately supports caller narrowing |
| `GroupResource.AllowAllNamespaces` | false | make all-namespaces access an explicit reviewable policy decision |
| `Gateway.Ordering` | strict | keep strict on supported Kubernetes; use lenient only for known aggregated APIs |

Snapshot object and byte limits remain a host-level scope policy concern. The gateway refuses to guess a
safe universal cap: object size, useful namespace size, and recovery behavior are product-specific.
Measure snapshot size and duration per allowed scope before opening broad all-namespaces endpoints.

## Counting lifetimes

`stream_opened`/`stream_closed` count `StreamProjection` entry/return, including authorization
failure, but exclude HTTP identity/scope refusals before entry. `shared_subscription_opened` and
`shared_subscription_closed` count active attachments, including warm-cache joins. Overflow, scope
death or leave ends an attachment once; repeated `Stop` does not count again. Cleanup may finish
later. A resnapshot can replace an attachment on one logical stream without another API watch.

Callbacks are synchronous and may run concurrently, including under shared locks. Return promptly;
do not panic or reenter the gateway. Update gauge counters synchronously rather than through a lossy
exporter queue. Opens precede matching closes, with no global order across independent lifetimes.
Ignore unknown kinds. Scope values are not safe metric labels. See the tested
[counter mapping](../gateway/kube/examples/sharedstream/metrics.go).

These gauges do not count HTTP requests still resolving identity or upstream watch handles. Keep
host metrics for those needs. Measure physical API-server WATCH requests independently with the
metrics available on the tested Kubernetes version; the shared-host fixture uses
`apiserver_longrunning_requests` filtered to the resource and `verb="WATCH"`.

## Bounded HTTP delivery

Set `WriteTimeout` explicitly; zero preserves no library-installed deadline and negative values
are rejected. Each header, event or heartbeat write and flush shares one deadline. Successful
operations clear it, so quiet streams can outlive many timeout periods. A failed operation cancels
its stream and cannot be retried as a terminal frame. Flush success does not acknowledge browser
receipt. Generic `Stream` sinks remain responsible for bounded I/O.

The transport owns write deadlines while serving and attempts to clear them at exit. It cannot
retrieve and restore a host's previous deadline. A whole-response `http.Server.WriteTimeout` is
not a per-operation substitute for a long-lived stream. Earlier request deadlines constrain I/O;
request cancellation may still wait for the current operation's bound. Backend opening/cleanup,
authorization gate waiting, encoding and host callbacks are outside that I/O budget.

With a positive bound, unsupported writers abort before streaming and emit `http_transport_rejected`.
No unbounded diagnostic frame is attempted; browser reconnects are possible. Test the actual mounted
middleware using the [capability-check recipe](../gateway/kube/examples/sharedstream/README.md#middleware-capability-test).
HTTP/1.1 sockets and TLS HTTP/2 (including two streams on one connection) are tested, along with
transparent `Unwrap` and opaque wrappers. Other middleware/proxy combinations need host tests.

For authorization bursts and the limits of the 200-subscriber profile, see the
[shared-host capacity guide](../gateway/kube/examples/sharedstream/README.md#verification-and-capacity).
