# Glossary for frontend developers

You do not need to know Kubernetes to use krm-stream. You do need about a dozen words. This page
defines them, then shows where each one appears in the library.

## The data

**KRM (Kubernetes Resource Model)** is the shape every object here has. It is JSON with four
conventions: `apiVersion`, `kind`, `metadata`, and a body. Read it as a schema for a declarative
object with an identity and a desired state. Nothing about it is specific to containers. A
`Database`, a `FeatureFlag` or a `Tenant` can be a KRM resource.

**Resource** is one such object. Identified by `apiVersion`, `kind`, `namespace` and `name`, and
uniquely by `metadata.uid`. The store keys on `uid` rather than name, because a delete and recreate
under the same name is a different object and must not inherit the old draft.

**CRD (Custom Resource Definition)** is how a team adds their own `kind`. It is why KRM works as an
application configuration API and not only as cluster plumbing: your product's domain objects can be
resources.

**`spec` and `status`** are the split to remember. `spec` is what a human or agent wants. `status`
is what the system observed. Users edit `spec`. Nobody edits `status`, and the store enforces that:
`status` is read-only and never part of a save.

**`resourceVersion`** is an opaque server-assigned token that changes on every write. It works like
an ETag, and it is how the server detects that you edited a stale copy.

**Namespace** is a folder. Resources live in one, and some kinds are cluster-wide instead.

## The stream

**Watch** is the Kubernetes primitive for "tell me when this changes". It yields a stream of `ADDED`,
`MODIFIED` and `DELETED` events. It is server-side and stateful, and it has edge cases: history
expiry, relist, bookmarks. The gateway handles those. You do not see them.

**Snapshot** is the initial run of events describing the world as it currently is, before live
changes arrive. It has a completion point, the `synced` event. Until a snapshot completes, the client
cannot tell whether a resource it remembers is gone or simply not re-sent yet, so an incomplete
snapshot never prunes state.

**SSE (Server-Sent Events)** is the browser transport: an HTTP response that stays open and streams
text events. `EventSource` is built into every browser. It is one-directional, which is all a read
stream needs.

**Gateway** is the server-side piece you mount in your own Go application. It holds the Kubernetes
credentials, decides who may see what, and turns a watch into a scoped SSE stream. The browser never
receives a cluster credential or an API-server URL.

**Projection** is the subset of a resource the gateway sends. What the browser receives may be less
than what exists upstream.

**Redaction** is a path the gateway knows exists but withholds, such as a Secret value. It is not
sent as a placeholder; it is absent from the object, and listed in `redactions(id)` so the UI can
render it as withheld and offer no editor. It follows that a redacted field must never be written
back, or the browser would erase it.

## The editing

**Draft** is the object your form is bound to. It is separate from the server object, and the stream
cannot overwrite it without telling you.

**Three-way merge**: while you are editing a resource, new server updates can arrive. A three-way
merge uses the version you started editing, your draft, and the new server version to combine
non-conflicting changes without overwriting your work.

At each editable path the store asks two questions: did the server change this field, and did you
change this field?

| Server changed it | You changed it | Result |
|---|---|---|
| yes | no | the server value flows into your draft |
| no | yes | your edit stands |
| yes | yes, to the same value | converge, no conflict |
| yes | yes, to a different value | your draft stands, and a conflict is recorded |

Only the last row needs a human. Without this, a controller updating one annotation would discard
the text you were typing in an unrelated field.

**Conflict** is the fourth row above. It is not an error and it does not block a save. The draft
still wins, and `conflicts(id)` gives the UI what it needs to show the server value alongside it,
with `takeTheirs` or `revert` as the ways out.

**Associative list** is a Kubernetes array that behaves as a map. `spec.containers` is keyed by
`name`, not by index. A merge that treats it as an array corrupts it when two people change
different containers. The store merges these by key.

**RFC 7386 merge patch** is the save format: a JSON document containing only what changed, where
`null` means delete. The store builds it by diffing draft against server over editable paths only.
It never diffs the whole projected object, which would turn a field that is absent because of a
projection into a deletion.

## How this hooks into krm-stream

The read path, in the order the words appear:

1. Your Go application mounts the **gateway**. It authenticates the user, decides the **scope**, and
   opens a **watch**.
2. The gateway applies a **projection** and its **redactions**, then streams a **snapshot** over
   **SSE**, followed by live updates.
3. `LiveResourceStore` consumes those events and keeps, per **`uid`**: `server(id)` for the server
   object, `draft(id)` for yours, plus `conflicts(id)` and `redactions(id)`.
4. Every incoming update runs the **three-way merge** over `server` as the base you started from,
   `draft` as what you typed, and the new server object. Server changes you did not touch appear in
   the form. Your edits survive. Collisions land in `conflicts(id)`.

The write path is not the library's:

5. On save, `patch(id)` returns an **RFC 7386 merge patch**, or `null` when nothing changed.
6. You send that to your own save endpoint. The store never writes to Kubernetes.
7. Your handler calls [`gateway.ValidateMergePatch`](../gateway/patch.go), which rejects a patch
   touching anything the effective projection withheld or stripped: a redacted path,
   `metadata.managedFields`, the last-applied annotation, and `status` under `ProjectionSpec`. It is
   what stops a buggy or hostile browser from destroying what it was never shown. Do not skip it on
   the grounds that the store is careful, because the store runs on the caller's machine.
8. The write goes to the API. The watch sees it, it returns down the stream as an ordinary update,
   and the merge converges your draft with it. Your own write needs no special handling.

If you know TanStack Query or SWR, this is the same server cache with local edits, with two
differences: the cache is pushed rather than refetched, and the local edit is not discarded when new
server data arrives.

Next: [Client state model](client-state-model.md) for the API surface, and
[Saving edits safely](saving.md) for the host's responsibilities on the write path.
