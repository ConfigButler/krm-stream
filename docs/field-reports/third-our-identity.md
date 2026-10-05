# Watch streams, optional editing and native access

**Request of 2026-10-05. Slice 1, the native viewer, is implemented; the follow-ups below remain
open.** It builds on the connector separation, Go API cleanup and real-API save tests. Use
[proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#open-work-and-delivery-order)
for completed work, delivery order and independent tracks.

## Identity and ownership

krm-stream provides live Kubernetes resource state for browser applications, with optional editing.
Its client adds connection lifecycle, recovery and a resource store. Its gateway adds named views,
redaction, suppression and optional upstream watch sharing. Editing adds drafts, reconciliation,
conflict review and captured save intent; the host owns every write.

| Source | Delivered content | Added value |
|---|---|---|
| Native through a host proxy — read-only viewing implemented | Original Kubernetes resources the host authorizes, including Secret values and machinery fields | Reuse connection lifecycle and live state without adopting the gateway |
| Gateway — supported | Named projected views delivered over SSE | Selected disclosure, fewer downstream events and optional shared upstream watches |

Native is the straightforward entry point for a host that already proxies Kubernetes. Gateway SSE
is the delivery format of the projected source, not a legacy label for its capabilities. Both sources
use fetch and share frontend lifecycle and state application. SSE and native JSON can both carry
errors; each connector classifies its own HTTP refusals and in-stream errors.

Scope, projection, suppression and sharing have separate costs and guarantees; see
[watching resources](../why-a-gateway.md). Full/spec redaction withholds core Secret values while
revealing paths and change revisions. It does not classify arbitrary sensitive fields in other kinds.
The host owns credentials, session validity, scope authorization, UI and resource limits.

## Native watch connector

**Adoption reason:** a host such as krm-foyer already exposes native collection URLs through its
session-authenticated `/k8s` proxy. Its page should reuse krm-stream lifecycle and state without first
wrapping native watch frames in SSE. Keep the projected gateway and its current v1 wire contract.

### Slice 1: a native viewer

**Implemented** as `connectNativeWatch` and `nativeCollectionURL`, with a shared internal lifecycle
used by both connectors, the [native viewer example](../../examples/native-viewer/README.md) and the
[client reference](../../packages/krm-stream/README.md#native-connections). The requirements below are
the contract it was built and tested against.

Deliver one small read-only connector, using the existing `ResourceStateEvent` consumer and
`LiveResourceStore(readOnlyPolicy)`. Keep the standalone `applyStreamEvent` API for this slice;
proposal 0009's bound-method/editor cleanup is independent. No second store, framework or native
write endpoint is needed. A short contract, executable example and focused tests belong in this
change; native editing and the larger comparison project do not gate it.

- Use caller-supplied fetch or global fetch, same-origin credentials by default and cancellable reads.
- Perform an ordinary collection LIST, then WATCH from its collection resourceVersion. Emit `reset`,
  an `added` for each member and `synced` for a complete snapshot, including an empty collection.
  Declare live only after snapshot application and watch acceptance; do not start the healthy-period
  timer while the watch request is still opening.
- Require a complete unpaginated LIST in slice 1. Do not request a page limit. If the response has a
  nonempty continuation token, refuse the unsupported partial collection with a clear terminal error
  before emitting `reset` or `synced`; it is not safe to prune from one page.
- Validate collection items and identity before delivery. Supply missing item type metadata from a
  concrete typed collection where valid, preserving fields already present; never guess kind from a
  plural resource URL or label an item with the collection's `List` kind.
- Decode native JSON frames across byte/chunk boundaries, including split UTF-8. Map resource events
  to added/modified/deleted and derive deletion identity from the object. Bookmarks affect neither
  membership nor an individual object's version; resume checkpoints are unnecessary for slice 1.
- Every reconnect starts a fresh LIST. HTTP/in-stream 410 triggers bounded recovery to a fresh
  snapshot. Do not route native 410 through the SSE transport's terminal HTTP classification.
- HTTP/in-stream 401/403 and other 4xx except 408/429/410 are terminal. Retry transient failures,
  408/429, 5xx, malformed/truncated frames and EOF with the existing bounded budget/backoff and hints.
  Repeated 410/short EOF must not create an immediate re-list loop.
- Share lifecycle and HTTP/error helpers where useful, without importing native framing through an
  SSE encoder/decoder. No SSE sequence or gateway protocol-header check belongs to native transport.
- Close/abort cancels LIST, WATCH, body reads and sleep; no subsequent events or live publication.
  Host consumer/subscriber/error-handler exceptions preserve existing cleanup and rejected completion.
- Use separate stores for separate sources, scopes and identities. Never fall back from a refused
  projected source to native access. Raw/native objects have no gateway projection or redaction revisions.

### Acceptance for slice 1

**Met.** Deterministic fake-fetch tests in `packages/krm-stream/test/native.test.ts` cover the list
below; the existing SSE lifecycle suite passes unchanged. `task e2e-browser` loads the example on both
entry points against a Playwright-played proxy, and `TestRealAPINativeWatchThroughHostProxy`
(`task test-real-api`) runs the connector through a credential-holding `/k8s` proxy against a real
API server: typed-list type metadata, modification, deletion, selector exit and same-name recreation.

Tests cover an empty collection; complete-list validation, unexpected continuation and type metadata;
chunked frames; malformed/truncated input; initial/list/watch failures; HTTP/in-stream 410 and auth
refusal; transient retries, exhaustion and retry hints; close during fetch/read/backoff; host callback
exceptions; UID replacement and missed deletes or selector exits repaired by re-list. Verify live
readiness, fresh snapshot pruning and preserved existing SSE lifecycle behavior.

Add a minimal native viewer with identity, current fields and connection state. It makes no save
claims and demonstrates cleanup. Show how to target an existing host proxy. If an existing real-API
harness can exercise LIST/WATCH cheaply, add one focused integration case and report its actual run;
a new cluster campaign is not a prerequisite. Existing tests still apply to any changed lifecycle.

## Follow-ups after the viewer

Each is separate from slice 1 and from the others:

- Native editing with machinery/last-applied protections and a source-bound write contract.
- Native resume, streaming lists and pagination.
- Comparative view and sharing measurements.
- Independent gateway continuation and save-progress evaluation.
- The independently proposed editor API cleanup in proposal 0009.

Native editing is a separate slice: define the editable policy, exclude machinery and last-applied
annotation changes, and bind host reads/writes to the same source and UID. Preserve later typing,
submitted intent and reconciliation guards. A native response must not recover a projected editor.
Keep [the editor model](../client-state-model.md) and [saving guide](../saving.md) as the detailed contracts.

Resumable native watches, streaming-list initialization and pagination are later improvements with
their own recovery tests. Slice 1 improves adoption and lifecycle reuse; re-listing on every reconnect
does not claim resume efficiency. Gateway upstream continuation has a different checkpoint owner and
can proceed independently. Neither implies browser replay for gateway SSE v1.

## Show the value of each path

After the viewer works, compare native/full/spec and shared/unshared gateway runs using the same
objects and churn. Demonstrate Secret disclosure, downstream bytes/events, notifications/renders,
snapshots, authorization work and upstream watches. Add editing once its native contract exists.
Keep this comparison and the larger benchmark outside slice 1; begin with the small viewer.

## Save progress under suppressed churn

[Proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#5-save-progress-under-suppressed-churn)
owns the independent evaluation of bounded submitted-intent recovery and optional coalesced version
delivery. Current projected recovery remains a guarded read, review and another deliberate Save.
It does not block the native viewer and does not authorize an automatic write policy in slice 1.
