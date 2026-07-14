# Proposal 0002: real-cluster verification

**Status:** implemented.

## Decision

Maintain a real Kubernetes test rung alongside fixture-based tests. It verifies the assumptions that a
scripted watch cannot prove: streaming-list boundaries, real SSE behavior, Kubernetes resource-version
semantics, and fallback behavior for aggregated APIs.

## Scope

- `task cluster-facts` records observed API behavior for the supported Kubernetes version.
- `task test-cluster` exercises the real gateway and `gateway/kube` backend against a cluster.
- The Kubernetes sample aggregated API remains part of the test environment because it can reject
  streaming lists even when the core API server supports them.

The backend therefore supports both streaming-list and list-then-watch at a pinned
`resourceVersion`. The latter is a compatibility path for APIs that cannot serve a streaming list.

## Non-goal

The real-cluster rung does not replace deterministic fixtures. Bookmarks, partial metadata, ambiguous
tombstones, and deliberately unorderable versions still need focused unit and conformance cases.

See [observed cluster facts](../facts/observed-v1.36.2+k3s1.md) and
[sample-apiserver setup](../../test/cluster/sample-apiserver/README.md).
