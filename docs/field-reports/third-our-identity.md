# Watch streams, optional editing and native access

**Current request, 2026-10-05.** Baseline: the stacked branch through PR #59 at `b5cc779`.
This document records requested work. The [README](../../README.md), [watching guide](../why-a-gateway.md),
[editor model](../client-state-model.md) and [saving guide](../saving.md) describe current use.
[Proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#open-work-and-delivery-order)
consolidates completed work, delivery order, dependencies and deferred items.

## Identity and ownership

krm-stream provides efficient live Kubernetes views for browser applications, with optional editing.
Start with the watch: select an authorized scope, choose a view, maintain live resource state and
recover safely. Add the editor when the page needs to preserve local work as the resource changes.

| Layer | Owns |
|---|---|
| Gateway | Scope and projection enforcement, redaction, suppression, optional upstream sharing and current SSE delivery |
| Connector | Fetch/body decoding, connection state, errors, cancellation and bounded recovery |
| Resource/editor store | Authoritative delivered objects, UID membership, snapshot pruning and optional draft reconciliation |
| Host application | Credentials, login/session lifecycle, authorization policy, UI, writes and domain progress |

Projection controls delivered fields; suppression reduces downstream events; sharing reduces duplicate
upstream watches. None replaces subscriber authorization or host resource limits. Full/spec views
withhold core Secret values but disclose their paths and change revisions. They do not automatically
redact every sensitive field in other resource kinds. See [watching resources](../why-a-gateway.md).

## Native watch connector

**Adoption reason:** the host already exposes native Kubernetes reads and watches through a
session-authenticated proxy. A browser should consume those watches with fetch and reuse the resource
lifecycle and editor without requiring an additional SSE wrapper. Native watch JSON is the preferred
transport direction. Retain gateway SSE as a compatibility path for consumers seeking that protocol,
and retain the projected gateway for redaction, suppression and watch sharing.

SSE itself can carry errors. The current fetch-based SSE connector already handles HTTP refusals,
in-stream errors, cancellation and bounded recovery. Browser `EventSource` has different header and
retry limitations. The native request simplifies integration and framing; it does not claim that
SSE cannot report an expired token. Compare error handling on both paths, including token/session
expiry, and stop a refused connection until the host restores access.

krm-foyer is the motivating host: its `/k8s` native proxy and `/stream/v1` gateway routes let a page
choose either source under a session. Host integration and dependency upgrades remain separate work.
Keep the Go gateway in this repository and preserve its existing v1 SSE contract.

### Contract before implementation

Provide a browser connector for a native Kubernetes collection URL through a host proxy. Use fetch,
including caller-supplied fetch and same-origin cookies, without Node polyfills or browser Kubernetes
credentials. Reuse the existing frontend lifecycle and state-event input. API names are not settled;
one lifecycle does not require pretending the two wire protocols are the same.

- Expose initialization, live state, stale state during reconnect, terminal refusal, explicit closure
  and retry exhaustion. Consumer callback failures follow the existing cleanup-and-reject rule.
- Decode native watch frames directly into state events; do not encode them as SSE to parse them again.
- Separate collection resume checkpoints from object edit preconditions. A bookmark cannot supply an
  individual object's edit version. Advance checkpoints only after successful state application.
- Represent native view identity explicitly, without inventing a gateway projection or redaction
  revisions. Retain native fields for viewing; exclude machinery fields from accidental edits,
  including the last-applied annotation. Define the host validation and native read/write contract.
- Require explicit source selection. Never retry a refused projected stream through native access.
  Source, target, scope, view or login-identity changes must not silently combine drafts or redactions.
- Preserve save guards, submitted intent and later typing. A no-op or superseded write cannot wait
  forever for an exact echo. Track dirty drafts, write acceptance and domain progress separately.

Start with a proposal and an executable example. Expose the connector only once recovery, state
membership and editing contracts have passed acceptance. Keep the supported gateway connector in
place until then; do not advertise a native API in adoption snippets.

### Recovery acceptance

| Scenario | Required result |
|---|---|
| Initial streaming list, including an empty collection | `reset`, initial members, then `synced` only at the initial-end bookmark |
| EOF/read failure before the initial boundary | Restart initialization; an individual initial object's RV does not prove collection completeness |
| Disconnect after initialization | Resume from the last applied event/bookmark; retain membership, with no reset or snapshot pruning for a successful resume |
| HTTP or in-stream 410 | Start a fresh snapshot; retain old state as stale until completion, then prune missed deletes |
| API explicitly lacks streaming-list support | Complete a paginated list, then watch from its collection RV; auth and transient failures are not feature refusal |
| HTTP or in-stream 401/403 | Terminal for this connection; no retry loop or source fallback; a later accepted login can create another connection |
| 429/5xx, network failure, repeated short EOF | Bounded backoff and retry budget honoring hints; no hot loop |
| Close while reading or sleeping | Cancel request/backoff, release reader and deliver no later updates |
| Chunked UTF-8/JSON, malformed/truncated frame | Parse complete frames across chunks; partial input never completes a snapshot |
| Delete/recreate with the same name | New UID is separate; late responses cannot resurrect the old resource |
| Label-scope entry/exit | Membership follows the watch, including removal without an upstream delete |

Cover repeated retry failure, expired list continuations, partial snapshots, scope/source changes and
cancellation during recovery. Do not assume periodic bookmarks: idle watches still need recovery.
Specify live/stale presentation during a successful resume without fabricating a `synced` snapshot.

## Show the value of each path

Use one small frontend and workload for native, `krm-full/v1` and `krm-spec/v1` views. Reuse rendering
and editing where their contracts permit it. Show a read-only view first, then an editor with local
image changes, unrelated replica changes and a real overlapping edit. No new dashboard or framework.

| Operation | Native | Full | Spec |
|---|---|---|---|
| Initial load / spec edit | Original resource | Projected resource | Projected resource without status |
| Status-only update | Update | Update | No object event |
| Managed-fields-only update | Update | No object event | No object event |
| Rotate a test Secret | Authorized reader gets value | Value absent; redaction revision changes | Value absent; redaction revision changes |
| Reconnect after delete | Resume or resnapshot | Snapshot prunes absent UID | Snapshot prunes absent UID |

Compare shared and unshared gateway runs separately, with identical scopes and multiple authorized
subscribers. Sharing is independent of view selection; a native/full/spec table alone cannot establish
upstream savings. Different scopes do not automatically share.

Use synthetic Secret values and assert their absence from full/spec transcripts, including after
reconnect. Verify unauthorized callers and expired sessions on both paths, and that shared identity
or fallback cannot grant access. A host's separately authorized native route can disclose Secret
values; view selection is not a universal field-permission system.

Measure downstream bytes/events, store notifications/renders, snapshot size and apply time, upstream
watches, gateway work and conditional-save outcomes under the same object counts and controller churn.
Report reconnect costs and authorization work. Suppression reduces downstream work; sharing reduces
upstream duplication; native resume can reduce snapshot work. Establish each benefit with measurements.

## Save progress under suppressed churn

A person editing spec should not repeatedly need a second Save when none of the relevant values
changed. Keep low browser traffic and real conflict protection. The current supported recovery is a
guarded projected read followed by review and another deliberate Save.

[Proposal 0006, save-progress evaluation](../proposals/0006-stream-and-save-implementation-plan.md#5-save-progress-under-suppressed-churn)
owns evaluation of bounded submitted-intent recovery and optional coalesced version delivery, reusing
the real-API fixtures. It specifies guards, compatibility, measurements and acceptance; this request
does not duplicate them or commit to a new wire event. The evaluation can retain today's baseline.

## Relationship to upstream continuation and adoption

Proposal 0006's upstream continuation runs inside the gateway's Kubernetes backend; the native
connector resumes a browser-owned watch through a proxy. They have different checkpoint owners and
authorization lifecycles. Share scenarios, not cursors. Neither adds browser resume to gateway SSE v1.

After the native connector is released, a host can upgrade and exercise both routes under the same
session, expiry and RBAC tests. Projected editors need projected recovery reads; a transparent native
proxy is not itself a replacement for that host contract. Current branch APIs and future requests
must remain distinct from a consumer's pinned dependency version.
