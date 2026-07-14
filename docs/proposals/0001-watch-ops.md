# Proposal 0001: conformance watch operations

**Status:** implemented.

## Decision

The conformance fixture language includes `bookmark`, `partial`, and `tombstone` watch operations in
addition to normal list, upsert, delete, relist, and disconnect operations.

| Operation | Required gateway behavior |
|---|---|
| `bookmark` | Absorb routine bookmarks. Only the initial-events-end bookmark produces `synced`. |
| `partial` | Reject metadata-only objects and begin a new snapshot cycle. |
| `tombstone` | Never guess a deleted object's identity; begin a new snapshot cycle instead. |

## Rationale

Each case is possible in Kubernetes or `client-go` and cannot be safely inferred from the usual watch
operations. Expressing them in shared fixtures keeps the gateway's producer behavior covered without
changing the browser protocol.

## Related behavior

The same work established arbitrary-size decimal `resourceVersion` comparison. Strict ordering rejects
unorderable values; `OrderingLenient` is the explicit compatibility mode for upstreams that cannot
provide that guarantee.

See [conformance/README.md](../../conformance/README.md) and [spec/v1.md](../../spec/v1.md).
