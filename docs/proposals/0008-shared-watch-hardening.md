# Proposal 0008: Shared-watch hardening

**Status: selected scope implemented.** This decision record replaces the old adopter issue inventory
and implementation handoff. Use [shared-watch authorization](../auth.md#shared-watch-authorization),
[the revocation budget](../auth.md#revocation-budget) and [operations](../operations.md) for current setup.

## Decisions and guarantees

| Problem | Implemented decision |
|---|---|
| A blocked upstream open held the shared backend lock and unrelated scopes | Open each scope independently outside the backend-wide lock, with a shared opening context and cancellable callers |
| The last waiting caller could leave an opening running | Last-waiter departure cancels and discards the opening; last-subscriber departure stops an established watch |
| A blocked HTTP write could delay timed authorization indefinitely | Positive timed reauthorization requires a positive HTTP write timeout, validated at construction/direct serving |
| Hosts needed exact authorization inputs for safe caching | Document and test the adapter's complete SubjectAccessReview request |

A caller's context bounds its own wait rather than the lifetime of other subscribers' shared watch.
An abandoned opening must not overwrite a replacement or publish its watcher; stop any discarded
watcher. Shared scope backoff and overflow resnapshot behavior remain unchanged.

`ReauthorizationTimeout` starts after the periodic check obtains the delivery gate. It bounds that
check's callbacks, not opening/cycle authorization or the entire revocation interval. Host callbacks
must honor contexts and generic sinks must bound delivery. A write timeout is necessary to bound
blocked HTTP I/O but does not make the total revocation budget equal to the callback timeout.

The kube adapter checks `list`, then `watch`, using the host-provided subject and the scope's group,
version, resource, namespace and name. Selectors and subresource are not review attributes. Target
selects the review client rather than a Kubernetes review field. Host caches must distinguish full
subject, request/verb and API-server authority, and retain session/projection checks.

## Deferred work and conditions for reconsideration

Public authorization recheck triggers, grouped per-principal/scope scheduling, decision caches and
exported attribute builders remain deferred. Revisit them only with measured duplicate requests or a
specific host lifecycle that existing request cancellation/timed checks cannot handle.

Any future cache needs complete subject/request/authority keys, explicit validity and invalidation,
failure behavior and subscriber isolation. Grouping must not let one expired session retain access
because another session still passes. Triggers need defined interaction with delivery gates, pending
checks, teardown and cancellation. Do not infer a cache or immediate revocation guarantee from sharing.

## Evidence and limits

Focused tests cover independent openings, caller/last-waiter cancellation, discarded/replaced opens,
shared scope lifetime, unsafe configuration refusal, blocked-reader bounds and exact review attributes.
Adopter reproductions motivated the changes; repository tests establish their own scope. These changes
add no new wire event, authorization framework or capacity claim. Actual runtime PR evidence remains
in history; [proposal 0007](0007-shared-stream-host-integration.md) retains the earlier transport decisions.
