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
| `consumer_resync` rising | upstream continuity was lost or a shared subscriber fell behind | correlate with API-server errors, shared overflows, and deployments |
| `shared_overflow` | one subscriber exceeded `SharedOptions.QueueDepth` | increase only after checking browser stalls and event rate; resnapshot is intentional |
| `terminal_error` | authorization, upstream, or protocol failure ended a stream | alert by low-cardinality error code; browsers must not retry terminal errors |
| `event_suppressed` ratio | `krm-spec/v1` is removing expected churn | a sharp drop may mean callers selected `krm-full/v1` or a projection changed |
| stream count / snapshot duration | connection pressure or oversized scopes | narrow namespaces/selectors; avoid accidental all-namespaces watches |
| unorderable `resourceVersion` terminal errors | an unsupported or aggregated API does not meet strict ordering | use `OrderingLenient` only after accepting the reduced monotonicity guarantee |

## Runtime controls

| Control | Default | Use |
|---|---:|---|
| `gateway.Options.HeartbeatInterval` | 20 seconds | set below the shortest proxy idle timeout |
| `gateway.SharedOptions.QueueDepth` | 256 live events | tune after measuring; it bounds memory per slow subscriber |
| `ScopePolicy.AllowLabelSelector` | false | enable only for an endpoint that deliberately supports caller narrowing |
| `GroupResource.AllowAllNamespaces` | false | make all-namespaces access an explicit reviewable policy decision |
| `Gateway.Ordering` | strict | keep strict on supported Kubernetes; use lenient only for known aggregated APIs |

Snapshot object and byte limits remain a host-level scope policy concern. The gateway refuses to guess a
safe universal cap: object size, useful namespace size, and recovery behavior are product-specific.
Measure snapshot size and duration per allowed scope before opening broad all-namespaces endpoints.
