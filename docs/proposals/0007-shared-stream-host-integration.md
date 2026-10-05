# Proposal 0007: Shared-stream transport correctness

**Status: implemented and released in 0.4.0.** This compact record preserves the decisions; current
configuration and boundaries are in [authorization](../auth.md), [operations](../operations.md) and
the [shared-host example](../../gateway/kube/examples/sharedstream/README.md).

## Decisions

| Area | Result and boundary |
|---|---|
| HTTP delivery | A configured positive `WriteTimeout` bounds each write-plus-flush operation, including headers, events and heartbeats; a quiet stream can outlive many timeout periods |
| Writer capabilities | Bounded serving requires flushing and write deadlines; unsupported middleware aborts before a logical stream opens |
| Lifecycle observations | Balanced stream and shared-subscription open/close events; bounded labels and synchronous, prompt callbacks |
| Host composition | Tested session/subject resolution, timed checks and write bounds in an example, rather than a public identity framework |

The library owns HTTP deadlines during serving, clears them after successful operations and at exit,
and cannot restore a previous host deadline. A whole-response server timeout is not a per-operation
bound. HTTP/1.1, TLS HTTP/2, peer isolation and transparent/opaque wrappers have focused tests; mounted
middleware and proxy combinations still need host checks. Generic sinks are the host's to bound.

Timed authorization and delivery share a subscriber gate. Write bounds matter to revocation, but
encoding, backend opening, gate waiting and arbitrary host callbacks are not bounded by the HTTP
write timeout. [The revocation budget](../auth.md#revocation-budget) owns that explanation.

Stream observations count `Stream` entry/return, including authorization refusal but excluding earlier
HTTP identity/scope refusals. Shared-subscription observations count attachments, including warm-cache
joins; overflow or scope loss can replace an attachment without a new logical stream. Callbacks may
run concurrently under shared locks, must return promptly and must not reenter the gateway.

## Capacity and deferred scope

The [recorded shared-host rehearsal](../facts/shared-host-rehearsal.md) reports 200 subscribers under
a declared workload. Its authorization rate is part of the cost: allowed checks require list and watch
reviews per subscriber. Watch sharing reduces upstream duplication, while snapshot delivery, browser
work and authorization still scale with subscribers. The rehearsal is not a universal capacity claim.

Public identity helpers, authorization caches, grouped scheduling and new metric APIs were not part
of this change. Use existing host wrappers and bounded labels. [Proposal 0008](0008-shared-watch-hardening.md)
records subsequent hardening; [proposal 0006](0006-stream-and-save-implementation-plan.md) owns remaining
continuation and save-progress evaluations. Native browser transport leaves these gateway boundaries
intact.
