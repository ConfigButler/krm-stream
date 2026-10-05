# Editor state model

Editing is an optional layer on a live resource view. `LiveResourceStore` keeps the last delivered
server object separate from the person's local draft, incorporates incoming changes and exposes
conflicts for review. The host owns the form and every write; see [saving edits safely](saving.md).
For a list or viewer today, use `LiveResourceStore(readOnlyPolicy)` and render `server(uid)`.
The connector consumes and delivers state events independently of the editor.

## Intended use

1. Choose one authorized source, scope and view for the store. Choose the form's editable fields.
2. Apply authoritative state events synchronously with `applyStreamEvent(store, event)`.
3. Render detached store reads and make edits through `setValue`, `removeKey` and related methods.
4. Present incoming changes and resolve conflicts explicitly while preserving the draft.
5. On a deliberate Save, capture the patch, UID and resource version together. Let the host authorize,
   validate and conditionally write that intent; preserve later typing during the request.
6. Observe the result through the watch or a guarded read under the same view. Track write acceptance
   and workload progress separately from draft dirtiness.

Do not mutate a `draft()` return value directly: reads are detached copies, and that bypasses edit
policy and notifications. Independent sources/scopes need separate stores. Switching identity or view
must not silently reuse drafts, redactions or snapshot state. The same store edits objects from
`connectNativeWatch`: native writes and recovery reads go through the proxy the watch reads, as
[native editing](saving.md#native-editing-through-a-host-proxy) describes. Never mix native and
projected objects, responses or editors in one store.

Both connectors feed the store through the existing standalone `applyStreamEvent(store, event)`. A
bound store method is proposed editor cleanup in proposal 0009, not a current API or a prerequisite
for native transport. No new store is needed.

## State per resource

| Value | Meaning |
|---|---|
| `server(id)` | The latest complete object delivered by the chosen source. Current gateway objects are projected; native objects retain upstream fields. Every upsert replaces it. |
| `draft(id)` | The object rendered and edited by the UI. Editable regions are reconciled with server changes. |
| `conflicts(id)` | Server values that changed concurrently with a different local edit. |
| `redactions(id)` | Paths known to exist upstream but intentionally withheld by the selected projection. |

The resource key is `metadata.uid`, not name. A delete followed by a recreate with the same name is a
new resource with a new draft.

## Stream lifecycle

Pass each state event from `connectResourceStream` to `applyStreamEvent` instead of translating
protocol events in UI code. The connector checks the sequence and handles `error` events itself, so
the store only sees state events.

- `reset` starts a snapshot and marks existing resources unseen.
- `added` and `modified` both replace the server object and reconcile the draft.
- `deleted` removes the resource and its draft.
- `synced` prunes resources that were not seen during the completed snapshot.
- `error` goes to the connector's `onError`, not to the store. It is terminal only when the event
  says it is terminal. `RESYNC_REQUIRED` starts a new
  snapshot on the same connection. After any other non-terminal error, such as
  `UPSTREAM_UNAVAILABLE`, the gateway closes the connection and the client reconnects, waiting at
  least `retryAfterMs`.

An incomplete snapshot never prunes state. That prevents a transient disconnect from making a UI lose
objects it has not yet reloaded.

## Editing and conflicts

The default editable regions are `spec`, `metadata.labels`, `metadata.annotations`, `data`, and
`stringData`. `status`, immutable metadata, and redacted paths are read-only.

Under every policy, the store also keeps `metadata.managedFields` and the
`kubectl.kubernetes.io/last-applied-configuration` annotation read-only. These are the machinery
that gateway projections remove and native objects carry. A map that holds a protected path, such
as `metadata.annotations` beside the last-applied annotation or a Secret's `data` beside withheld
values, cannot be replaced or removed whole. It is merged and edited key by key instead.

Suppose a Deployment starts with image `v1` and replicas `3`. The person types image `v2`, then an
autoscaler changes replicas to `5`. The draft becomes image `v2`, replicas `5`: untouched fields follow
the server while local work stays. If another person changes the image to `v3`, the local image `v2`
stays and a conflict records `v3` for review.

When a new server object arrives, the store compares three values at each editable path: **base** is
the previous server value, **ours** is the draft, and **theirs** is the incoming server value. The
incoming object then becomes the new server base.

| Draft differs from base | Incoming server differs from base | Result |
|---|---|---|
| no | yes | follow the server |
| yes | no | keep the draft |
| yes | yes, matching the draft | converge and clear conflict |
| yes | yes, differing from the draft | keep the draft and record a conflict |

`isDirty` and `changes` are derived from `draft` versus `server`; neither is a cache that can drift
after a stream update. `revert` restores the current server value. To keep a local value explicitly,
use the [tested keep-local recipe](../examples/editor-recipes/README.md#keep-the-local-value-in-a-conflict).
The UI owns the choice; the store does not silently choose a winner for differing concurrent edits.

## Patches and redactions

`patch(id)` returns an RFC 7386 JSON merge patch over editable changes, or `null` when there are no
changes. It never diffs the full projected object, so a field absent because of a projection cannot
accidentally become a deletion.

Redacted paths are not placeholders and do not appear in the object. Render a withheld value using
`redactions(id)`, and do not offer an editor for it. The host save endpoint must also call
[`gateway.ValidateMergePatch`](../gateway/patch.go) before writing to Kubernetes.

## Creating and deleting whole objects

The store holds only objects with a server identity — a `metadata.uid` — whether the stream delivered
them or `adoptSaved` inserted one from a save response before its echo arrived. That limit is
deliberate, and it is why two operations do not live here:

- A pending **create** has no server object, so no uid and no key. There is nothing to merge it
  against; `changes()`, `patch()`, and `conflicts()` have no meaning for it.
- A **delete** has no fields to reconcile.

Staging a pending create or delete is therefore the consumer's job, the same way the write itself is
(see [saving edits safely](saving.md)). Keep them in page-local state and render all three sources as
one review list under one Save:

```ts
const pendingCreates = []; // client-only drafts keyed by a local draftId, never a uid: { draftId, name, data }
const pendingDeletes = new Set(); // uids marked for removal

// One list, one Save:
//   pendingCreates      → "create <name>"
//   pendingDeletes      → "delete <name>"
//   store.changes(uid)  → field edits   (skip a uid that is in pendingDeletes)
```

### Reflecting the result

Prefer 204 or a receipt and let the stream reflect creates and deletes. Keep accepted writes separate
from pending confirmation, so a delayed watch echo cannot cause a duplicate submission. A create
arrives with its server-assigned UID; a delete removes only the original UID. A completed snapshot
can confirm missed observations. Account for an echo arriving before the HTTP response as well as
for an echo arriving afterwards.

Current `adoptSaved` and `removeResource` APIs remain available, but the recommended flow uses the
stream. `adoptSaved` is unguarded and must not receive an asynchronous response that can race newer
watch state; a reconciliation guard for an existing UID does not insert a newly created resource.
Optimistic removal after a failed delete cannot recover until an authoritative event or snapshot
restores the object. Do not invent UIDs or fabricate authoritative stream events to represent intent.

Deletion and snapshot pruning discard the resource's draft. If recovery matters, capture a detached
copy as edits change, before removal, using the [recovery-copy recipe](../examples/editor-recipes/README.md#recover-work-after-a-deletion).
Retain it under the original identity and UID for explicit copy-out; never transfer it automatically
to a same-name replacement.

## Arrays and associative lists

Arrays are atomic by default. A concurrent array change conflicts with a local array edit, which is
safe for ordinary JSON arrays where position is not stable.

Kubernetes associative lists can merge by identity when the host provides the structural OpenAPI
schema for the exact GroupVersionKind:

```ts
import { defaultPolicy, LiveResourceStore, withOpenAPIKeyedLists } from "@configbutler/krm-stream";

const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, deploymentSchema));
```

`withOpenAPIKeyedLists` recognizes `x-kubernetes-list-type: map` with
`x-kubernetes-list-map-keys`. It preserves an edit to one keyed item across server reorders and
unrelated keyed-item updates. Missing keys, duplicate keys, malformed lists, and unannotated arrays
retain the atomic behavior. Merge patches still send the final array as one RFC 7386 value.

## UI integration

The store has no rendering dependency. Subscribe once, then query `draft`, `server`, `changes`,
`conflicts` and `redactions` during rendering. Connection state belongs to the connector:

```ts
const unsubscribe = store.subscribe(() => render(store));
store.setValue(uid, ["spec", "replicas"], 3);

// Save from an explicit user action while the connection is live.
const intent = store.captureSave(uid);
if (intent) await hostSave(intent);
```

Use the [conditional editor](../examples/conditional-save/README.md) for conflict checks, serialized
saves and guarded asynchronous responses. `adoptSaved` is for synchronous adoption or newly created
objects; a delayed response must use a reconciliation guard. See [saving](saving.md).

A clean draft means it matches the delivered editable values. It does not prove that a captured write
was accepted or that a controller completed its work. A suppressed update can leave the displayed
view unchanged but its save version stale; see [the save-version explanation](saving.md#why-a-quiet-stream-can-still-reject-a-save).
For reactive integration and subscription ownership, see the [Vue example](../examples/vue/README.md).
