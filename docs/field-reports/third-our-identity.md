# Watch streams, optional editing and native access

**Request of 2026-10-05. Slice 1, the native viewer, slice 2, native editing, and native watch
resumption are implemented; the other follow-ups below remain open.** It builds on the connector separation, Go API cleanup and real-API save tests. Use
[proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#open-work-and-delivery-order)
for completed work, delivery order and independent tracks.

## Identity and ownership

krm-stream provides live Kubernetes resource state for browser applications, with optional editing.
Its client adds connection lifecycle, recovery and a resource store. Its gateway adds named views,
redaction, suppression and optional upstream watch sharing. Editing adds drafts, reconciliation,
conflict review and captured save intent; the host owns every write.

| Source | Delivered content | Added value |
|---|---|---|
| Native through a host proxy — viewing and editing implemented | Original Kubernetes resources the host authorizes, including Secret values and machinery fields | Reuse connection lifecycle and live state without adopting the gateway |
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

### Slice 2: native editing

**Implemented** as store-level machinery protection, `nativeObjectURL`,
`gateway.ValidateNativeMergePatch` and the [native editor example](../../examples/native-editor/README.md),
with the contract in [saving](../saving.md#native-editing-through-a-host-proxy). The requirements
below are what it was built and tested against.

- **Edit policy.** No new policy and no new store. `metadata.managedFields` and the
  `kubectl.kubernetes.io/last-applied-configuration` annotation are read-only under every policy, as
  redacted paths are: the same two paths `ValidateMergePatch` refuses for every projection. A map
  holding a protected path cannot be replaced or removed whole; where the policy makes it editable it
  is merged and edited key by key. This also makes a new Secret key beside redacted values reach the
  patch, which it previously did not.
- **Source binding.** Writes and recovery reads address the object through the same proxy and
  collection the watch reads (`nativeObjectURL`), under the object's own namespace and name. A
  recovery read must be a Kubernetes object with the editor's UID; a projected envelope or another
  UID is never reconciled. A native response never reaches a projected editor.
- **Write contract.** The browser sends the captured intent as `application/merge-patch+json` with
  `metadata.uid` and `metadata.resourceVersion` from the capture, never from a newer read. The host
  proxy accepts only that content type, bounds the body and runs `ValidateNativeMergePatch`, which
  refuses a patch missing either precondition or touching machinery. The host keeps authentication,
  write authorization and audit; Kubernetes RBAC still applies.
- **Save guards.** Unchanged: atomic capture, serialized saves, writes only while live, no adoption
  of the write response, 409 recovery through `captureReconciliation`, a refused read owing another
  read, and later typing preserved. A 422 on `metadata.uid` is treated as a replaced object.
- **Unknown outcomes and confirmation.** After a network failure or a 5xx, or an accepted write
  whose echo has not arrived, the next Save is a guarded read, never a second PATCH; `confirm()`
  performs that read explicitly when no echo arrives. A 4xx is a definite refusal and owes no read.

### Acceptance for slice 2

**Met.** `packages/krm-stream/test/invariants.test.ts` covers machinery under the default policy and
under a policy that makes all of `metadata` editable, and a Secret key added beside redacted values.
`packages/krm-stream/test/native-editing.test.ts` covers the request shape and URL, machinery
rewritten by the server during editing, 409 recovery to `version-stale` and to `draft-conflict`,
replacement by 409 and by a `metadata.uid` 422, deletion, refused writes keeping their Status, reads
overtaken by the watch, the live check and a projected envelope, an unknown write outcome (network
failure and 502) settled by a read, a definite 422 needing none, and confirmation by echo, by the
next Save's read and by `confirm()` after a write a webhook reverted. `gateway/patch_test.go` covers
`ValidateNativeMergePatch`.

`TestRealAPINativeEditThroughHostProxy` (`task test-real-api`) runs the real connector and editor
through a credential-holding `/k8s` proxy that validates every PATCH, against a real API server: a
save and its watch echo with the last-applied annotation untouched, a competing write landed between
capture and PATCH (409, guarded read, a second deliberate save, neither write lost), the proxy
refusing an unconditional patch and a machinery patch before they reach the API server, and an
object replaced under the same name reported `unavailable` with the replacement untouched.

**Browser acceptance (2026-10-05).** The [example page](../../examples/native-editor/README.md#run-the-page)
edits one ConfigMap with the same editor, store and connector, built from `editor.ts` without a
second save controller or new library API. `examples/vanilla-browser/tests/native-editor.spec.ts`
(`task e2e-browser`) runs it in Chromium on both entry points, with the watch live throughout, against
a test server playing the host proxy. It covers a save and its echo, typing and focus during a save,
409 recovery and a second deliberate save, explicit conflict resolution, a lost response and a 502
each settled by a GET before any further PATCH, an accepted write without an echo confirmed
explicitly, 403 and 422 refusals, multiline values, deletion and UID replacement with copy-out, and
disconnect cleanup. A manual run through `kubectl proxy` against the v1.36.4+k3s1 spike cluster
saved and echoed on both entry points and rendered a real 422. The comparative frontend (order 6) remains separate.

## Follow-ups after the viewer

Each is separate from the slices above and from the others:

- Native resume — **implemented**, see [below](#native-watch-resumption); streaming lists and
  pagination remain open.
- Comparative view and sharing measurements.
- Independent gateway continuation and save-progress evaluation.
- The independently proposed editor API cleanup in proposal 0009.

Slice 1 re-listed on every reconnect and claimed no resume efficiency; the requirements above are
the contract it was built against. Gateway upstream continuation has a different checkpoint owner and
can proceed independently. Neither implies browser replay for gateway SSE v1.

### Native watch resumption

**Implemented** in `connectNativeWatch`; the contract is in the
[client reference](../../packages/krm-stream/README.md#native-connections).

- Each handle keeps a private checkpoint, armed only once a complete snapshot was applied and its
  WATCH accepted, and advanced only after the consumer applied an event or the stream passed a
  bookmark. It is never exposed, accepted or shared, so it stays bound to the handle's proxy, scope,
  selectors and credentials. Resource versions are opaque and replaced in stream order. Bookmarks
  (`allowWatchBookmarks=true`) move only the checkpoint, never an object's write version.
- EOF, network failures, a truncated final frame and retryable refusals (408, 429, 5xx, HTTP or in
  the stream) resume the WATCH from the checkpoint: no LIST, `reset` or `synced`, so membership,
  drafts and conflicts survive and missed updates, deletes, selector exits and same-name UID
  replacements arrive as watch events. The state goes `connecting` → `live` once the resumed watch is
  accepted.
- HTTP or in-stream 410, malformed frames, an error without a code and an event without a
  resourceVersion discard the checkpoint; the next connection is a fresh, complete LIST/WATCH
  snapshot, during which the state is not live and editors refuse to save. Terminal refusals, the
  bounded retry budget, cancellation and callback-error cleanup are unchanged; every reconnect still
  consumes the budget.

**Acceptance.** `packages/krm-stream/test/native.test.ts` covers missed updates, deletes, selector
exits and UID replacement replayed on resume, bookmarks, opaque versions, split and truncated frames,
malformed input, interrupted initialization, HTTP and in-stream expiry, repeated expiry to
exhaustion, terminal and transient refusals on a resumed watch, cancellation, per-handle checkpoints
and drafts/conflicts across a resume and a re-list; `native-editing.test.ts` covers a save refused
during expiry recovery. `task e2e-browser` runs the viewer and editor pages through resume and an
in-stream 410 on both entry points. `TestRealAPINativeResumeThroughHostProxy` (`task test-real-api`,
v1.36.4+k3s1) drops the watch at a host proxy, mutates the cluster while the next WATCH is held and
converges with no second LIST; it then rewrites a WATCH's resourceVersion to `1` so the real API server
answers with its own in-stream 410, and recovers through a fresh snapshot, twice in a row. The expiry
is induced by the proxy; the 410 is the server's.

**Limits.** A resumed watch is live on acceptance, so an expired one is briefly live until its
in-stream 410 arrives; a resume does not invalidate a guarded read as a snapshot does (an event for
the object does), as on any live watch; the checkpoint is in memory and a page reload lists again.
Streaming-list initialization and pagination are evaluated in
[proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#3-native-streaming-lists-and-pagination)
and not implemented.

## Show the value of each path

After the viewer works, compare native/full/spec and shared/unshared gateway runs using the same
objects and churn. Demonstrate Secret disclosure, downstream bytes/events, notifications/renders,
snapshots, authorization work and upstream watches. Include editing on both sources.
Keep this comparison and the larger benchmark outside slice 1; begin with the small viewer.

## Save progress under suppressed churn

[Proposal 0006](../proposals/0006-stream-and-save-implementation-plan.md#5-save-progress-under-suppressed-churn)
owns the independent evaluation of bounded submitted-intent recovery and optional coalesced version
delivery. Current projected recovery remains a guarded read, review and another deliberate Save.
It does not block the native viewer and does not authorize an automatic write policy in slice 1.
