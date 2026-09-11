# Kubernetes API reference notes

Source: [Kubernetes API concepts](https://kubernetes.io/docs/reference/using-api/api-concepts/),
reviewed from upstream markdown on **2026-07-11**. These notes explain the upstream assumptions behind
[the protocol](../../spec/v1.md). They are separate from the
[recorded v1.36.2 cluster observations](observed-v1.36.2+k3s1.md), which establish behavior only for
that tested environment.

## Watch events and partial objects

A watch sends JSON notifications with `type` and `object`. `ADDED`, `MODIFIED` and `DELETED` describe
resources; synthetic `ADDED` events can establish initial state.

[Bookmarks](https://kubernetes.io/docs/reference/using-api/api-concepts/#watch-bookmarks) carry a
resource-version checkpoint, not a complete resource. They are opt-in, have no guaranteed cadence
and need not arrive even when requested. A gateway must absorb them rather than replace a consumer's
object with their partial payload.

Metadata-only requests can return `meta.k8s.io/v1 PartialObjectMetadata`, including a UID. A missing
UID is therefore not the only partial-object signal: the gateway also checks the kind. Forwarding
such an object as an upsert would erase the resource's visible body.

An `ERROR` watch event is defined in `apimachinery` and can carry a Kubernetes Status. The recorded
cluster run verifies that an expired watch revision arrives as `ERROR` with code 410 (F3). Informer
`DeletedFinalStateUnknown` tombstones are a `client-go` construct, distinct from API-server deletes.

## Resource-version ordering

The reference defines ordering within the same API group and resource type using arbitrary-size
decimal integers. Compare length first, then lexicographically for equal lengths; a fixed-width
integer parse is insufficient. Do not compare a Pod's version with a Deployment's version.

For Kubernetes 1.35+, orderable resource versions are a conformance requirement for built-in and
custom resources. Extension/aggregated APIs have a separate caveat: non-decimal versions can be
checked for equality but cannot be reliably ordered.

| Upstream | Gateway choice |
|---|---|
| Supported conformant Kubernetes API | Default `OrderingStrict`; refuse an unorderable version. |
| Known aggregated API without orderable versions | Explicit `OrderingLenient`; do not drop updates whose order cannot be established. |

Browser consumers treat all resource versions as opaque strings. A collection's resourceVersion
marks the list boundary; it is not the version of any particular item.

The `resourceversion-bignum` and `resourceversion-unorderable` fixtures use an aggregated `Flunder`
to test these boundaries. The observed sample-apiserver used small decimal versions (F4/F6), so the
real-cluster run does not replace either fixture.

## Streaming lists and snapshot completion

The reference's streaming-list request uses:

- `sendInitialEvents=true`;
- `resourceVersionMatch=NotOlderThan`;
- `allowWatchBookmarks=true`;
- an empty or absent resourceVersion for a consistent initial read.

Synthetic `ADDED` events establish the snapshot, followed by its terminating bookmark and live
updates. The adapter recognizes the boundary using
`metadata.annotations["k8s.io/initial-events-end"] == "true"`, defined by
`metav1.InitialEventsAnnotationKey` in `apimachinery`. Observation F1 verifies the marker on v1.36.2;
it is not established by the API concepts page alone.

Observation F6 shows the sample aggregated API rejecting `sendInitialEvents`. The adapter therefore
also supports list-then-watch at the collection resourceVersion, synthesizing the snapshot boundary
from the completed list. The fallback is needed for aggregated APIs as well as older configurations.

## Lost continuity

When retained history no longer contains a requested revision, Kubernetes returns `410 Gone` and the
client must reinitialize from a fresh read. The gateway maps continuity loss to `reset` … `synced`;
the browser retains its old entries until the new snapshot completes and only then prunes unseen UIDs.

A watch opened at `resourceVersion="0"` can serve stale state and rewind. The adapter avoids that
mode. Downstream v1 connections always begin with a snapshot; browser object versions do not provide
resume. Upstream continuation remains tracked in the
[work plan](../proposals/0006-stream-and-save-implementation-plan.md#4-measured-upstream-continuation).

## Deletion

An object with finalizers may first arrive as `MODIFIED` carrying `deletionTimestamp` and finalizers,
then later as `DELETED`. A UI can render the terminating state through ordinary object updates.
Observation F7 records a complete deleted object with a UID. A missing or ambiguous tombstone UID
still requires snapshot recovery; the gateway never guesses identity.

## Writes

| Content type | Operation |
|---|---|
| `application/apply-patch+yaml` | Server-side apply |
| `application/json-patch+json` | RFC 6902 JSON Patch |
| `application/merge-patch+json` | RFC 7386 merge patch, produced by the client store |
| `application/strategic-merge-patch+json` | Kubernetes-specific strategic merge patch; unavailable for CRDs |

Whole-object PUT can lose fields the client omitted. PATCH can carry a resourceVersion precondition
against lost updates. The host must capture the patch and its version together, validate the active
projection and handle stale-write rejection; see [saving](../saving.md).

Local keyed-list reconciliation uses host-supplied structural OpenAPI metadata. It does not change
the wire save format: RFC 7386 still replaces arrays in full. The field-ownership and omission rules
for apply are separate; see [SSA tradeoffs](../proposals/0005-kubernetes-stream-and-save-semantics.md#why-ssa-is-an-option-not-a-replacement-guarantee).

## Executable evidence

The [conformance corpus](../../conformance/README.md) covers framing, projection and deliberately
adversarial watch inputs. `task cluster-facts` records API observations; `task test-cluster` exercises
the adapter against both streaming-list and aggregated-API fallback paths. See
[verification tasks](../../CONTRIBUTING.md#test-levels) for their prerequisites and limits.
