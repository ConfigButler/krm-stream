# Proposal 0009: Stream and editor separation

**Status: partly implemented on the stacked branch through `b5cc779`.** One event-based connector,
host-callback failure handling and the adopted-save snapshot-membership fix are implemented.
Editor API cleanup remains proposed. A dedicated read-only store and separate entry points remain
deferred. Current APIs are documented in the [client README](../../packages/krm-stream/README.md) and
[upgrade guide](../migrating.md); this proposal does not advertise future APIs as available.

## Direction and ownership

Start with watch streams and live resource state; editing is an optional layer. Keep one repository,
one npm package, zero browser runtime dependencies and lockstep releases. Follow the
[design rules](../../CONTRIBUTING.md#design-rules) and [release policy](../releasing.md).

The connector owns decoding, ordering, connection state, cancellation and bounded recovery. State
application owns complete resources, UID membership and snapshot pruning. The editor adds drafts,
edit policy, three-way reconciliation, conflicts and patch capture. The host owns authentication,
authorization policy, credentials, UI, writes and deletion recovery copies.

The [editor model](../client-state-model.md) explains why a moving server copy and local draft belong
together; [saving](../saving.md) owns the conditional-write contract. Keep those explanations there
rather than duplicating them in this implementation plan.

The [native-watch request](../field-reports/third-our-identity.md#native-watch-connector) extends this
separation: prefer fetch consuming native Kubernetes frames where the host provides a proxy, with
SSE retained as a compatibility path and the current gateway delivery format. Share lifecycle and
state application without converting native JSON to SSE. Native support is not implemented here.

## Implemented connector and correctness fixes

`connectResourceStream(url, consume, options)` is the one current connector. `consume` receives
`ResourceStateEvent` synchronously and independently of a store. It can call the standalone
`applyStreamEvent(store, event)` or another state consumer. The connector handles wire sequence and
errors privately; its handle exposes `state`, `subscribe`, `close` and `closed`.

- Reset publishes `syncing` before application; `synced` is applied before publishing `live`.
- Sequence gaps discard the event beyond the gap and recover through a fresh snapshot. Gap details
  appear on the resulting `retrying` state. Unknown wire events still participate in sequencing.
- HTTP refusals and in-stream errors preserve terminal/retry classifications and retry hints.
- Consumer, state-subscriber or error-handler exceptions stop recovery, finish cleanup and reject
  `closed` with the first host exception. They must not become network retry storms.
- Close/abort prevents later events, including remaining events in a decoded chunk. Close during
  `synced` prevents a later `live` publication.
- A mismatched optional protocol header is terminal before event application; an absent header is
  accepted. No wire negotiation was added.
- Adopted save responses no longer mark snapshot membership. Only authoritative stream upserts
  establish membership; a late response cannot preserve a UID absent from a completed snapshot.

The callback must finish synchronously. An async callback type-checks but cannot establish these
application-before-live and exception guarantees. Callers handle `closed` rejection and dispose all
subscriptions. Native `EventSource` framing/cookie compatibility remains tested separately from the
reference fetch connector's retry policy.

## Proposed editor cleanup

Keep reconciliation behavior and the gateway v1 contract. Reduce public input and duplicate exports
as one separately reviewable breaking change:

| Area | Proposed change |
|---|---|
| State input | Bound `store.applyStreamEvent(event)`; private snapshot/upsert/removal helpers; remove standalone `applyStreamEvent`, `ApplyOptions` and duplicate result types |
| Save adoption | Remove unguarded `adoptSaved` and public optimistic `removeResource`; existing-resource asynchronous responses use captured reconciliation guards |
| Redaction reads | Simplify `ReconciliationOptions` to `{ redactedPaths?: string[] }`; keep revisions on authoritative stream events |
| Duplicate helpers | Remove `takeTheirs` in favor of `revert`, and `status(uid)` in favor of `server(uid).status` |
| Root exports | Keep domain APIs, policies and schema helpers; make cloning/path/decoder/sequence implementation utilities private |

Reconciliation captures UID, resource revision and snapshot generation before a request. Refuse a
response after a newer base, during or across snapshot recovery, for a deleted/replaced UID, or with
unknown redaction paths. Preserve typing after capture. Omitted redaction paths keep protections;
explicit paths remove absent protections and preserve known revisions, never inventing counters.

Successful edits normally converge through the watch or a guarded projected read. Creates/deletes
remain host-owned intents and accepted-but-pending confirmations until an echo or completed snapshot.
Account for either echo/response order and prevent duplicate submissions. A guard does not insert a
new resource. Failed writes leave authoritative state intact; do not fabricate events or UIDs.

Keep deletion recovery and keep-local recipes on store APIs. `StreamChange` should report actual
removals from delete/snapshot completion, structural changes, highlights and conflicts; repeated
replay must not report a second removal. Retain the adopted-save regression until adoption is removed.

## Deferred until a read-only view needs them

A viewer currently uses `LiveResourceStore(readOnlyPolicy)` and carries the editor implementation.
Add `ResourceStore` only for a demonstrated list/viewer need. It would expose event application,
`ids`, detached `server`/`redactions` reads and subscription, without drafts or merge APIs. Complete
upserts replace objects; only a completed snapshot prunes unseen UIDs; replay stays idempotent.

Extract a shared internal snapshot tracker when the second store needs it. Its generation, seen set
and begin/finish logic must distinguish authoritative stream membership from guarded reads. Each
store owns one source/scope and has its own tracker; an editor does not mirror a second resource store.

Separate `stream` and `editor` package entry points remain deferred until an unbundled consumer
benefits measurably from avoiding editor modules. Keep the combined root and single-file bundle.
When implemented, assert the emitted graph: stream imports load no editor/merge/schema implementation,
and editor imports load no connector. Audit effects before declaring `sideEffects: false`; no new
stream-only bundle is justified yet. Existing `readOnlyPolicy` stays exported until a dedicated
viewer store replaces its role.

## Acceptance and migration

Implement cleanup independently of native transport, gateway fixes and real-API evidence. Inventory
repository and available consumer imports; unavailable source does not establish no users. Remove
superseded APIs before 1.0 without aliases, update all examples and extend [migration](../migrating.md).
Do not change release-generated versions or changelogs by hand.

Verify observable behavior through public events: complete replacements, redactions, UID changes,
partial/restarted snapshots, deletion during disconnect, later typing, late reads across snapshots,
conflict retention and keyed lists. Verify connection cleanup, callback exceptions, gap recovery,
retry hints/exhaustion and protocol diagnostics. Host recipes must preserve intent, confirmation and
no-duplicate-submission behavior. Packed exports and declarations must match the supported API.

For runtime changes run `task fixtures-check`, `task test`, `task lint`, `task e2e-wire`,
`task e2e-browser` and `task pack-client`, then inspect final-commit CI. Entry-point graph and dual-store
checks apply only when their deferred changes are implemented. Documentation edits need link and
example/diagram review, not a cluster campaign.

[Proposal 0006](0006-stream-and-save-implementation-plan.md) owns gateway continuation and save-progress
evaluation. [Proposal 0010](0010-gateway-api-cleanup.md) records implemented Go cleanup. Completed
shared-watch hardening does not reopen authorization triggers or caches without measured demand.
