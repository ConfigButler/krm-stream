# Client state model

`LiveResourceStore` is a headless TypeScript store for a KRM resource stream. It separates the
authoritative object received from Kubernetes from the user's local draft, so a live update never
silently overwrites an edit.

## State per resource

| Value | Meaning |
|---|---|
| `server(id)` | The latest complete projected object. Every stream update replaces it. |
| `draft(id)` | The object rendered and edited by the UI. Editable regions are reconciled with server changes. |
| `conflicts(id)` | Server values that changed concurrently with a different local edit. |
| `redactions(id)` | Paths known to exist upstream but intentionally withheld by the selected projection. |

The resource key is `metadata.uid`, not name. A delete followed by a recreate with the same name is a
new resource with a new draft.

## Stream lifecycle

Use `applyStreamEvent` or the transport helpers instead of translating protocol events in UI code.

- `reset` starts a snapshot and marks existing resources unseen.
- `added` and `modified` both replace the server object and reconcile the draft.
- `deleted` removes the resource and its draft.
- `synced` prunes resources that were not seen during the completed snapshot.
- `error` is terminal only when the event says it is terminal. `RESYNC_REQUIRED` starts a new
  snapshot on the same connection.

An incomplete snapshot never prunes state. That prevents a transient disconnect from making a UI lose
objects it has not yet reloaded.

## Editing and conflicts

The default editable regions are `spec`, `metadata.labels`, `metadata.annotations`, `data`, and
`stringData`. `status`, immutable metadata, and redacted paths are read-only.

When a new server object arrives, the store compares three values at each editable path:

| Base | Draft | Incoming server | Result |
|---|---|---|---|
| unchanged | any | changed | follow the server |
| changed | local edit | unchanged | keep the draft |
| changed | same value | same value | converge and clear conflict |
| changed | different value | different value | keep the draft and record a conflict |

`isDirty` and `changes` are derived from `draft` versus `server`; neither is a cache that can drift
after a stream update. `revert` or `takeTheirs` restores the current server value.

## Patches and redactions

`patch(id)` returns an RFC 7386 JSON merge patch over editable changes, or `null` when there are no
changes. It never diffs the full projected object, so a field absent because of a projection cannot
accidentally become a deletion.

Redacted paths are not placeholders and do not appear in the object. Render a withheld value using
`redactions(id)`, and do not offer an editor for it. The host save endpoint must also call
[`gateway.ValidateMergePatch`](../gateway/patch.go) before writing to Kubernetes.

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

The store has no rendering dependency. Subscribe once, then query `draft`, `status`, `changes`,
`conflicts`, and `redactions` during rendering:

```ts
const unsubscribe = store.subscribe(() => render(store));
store.setValue(uid, ["spec", "replicas"], 3);

const patch = store.patch(uid);
if (patch) await save(patch);
```

Use `adoptSaved` with the object returned by a successful host save to clear local dirtiness before
the watch echo arrives. See [`packages/krm-stream/`](../packages/krm-stream/) for the public API and
[`conformance/`](../conformance/) for executable behavior examples.
