# Editor recipes

Two small pieces of host code for situations the store deliberately leaves to the host. Copy them
into your application and change their library import to `@configbutler/krm-stream`. They use only
the store's public methods, and [their tests](../../packages/krm-stream/test/recipes.test.ts) run
with `task test`, driving the store with stream events as `connectResourceStream` delivers them.

## Recover work after a deletion

A `deleted` event, or a snapshot that no longer contains an object, removes the object and its draft
from the store. Once that has happened there is no draft left to read, so a copy must be taken while
the object still exists. [`retainRecoveryCopy`](recoveryCopy.ts) keeps a detached copy of one fixed
UID's draft and unsaved edits, refreshed on every store notification from the moment it starts:

```ts
const recovery = retainRecoveryCopy(store, uid, { retainMs: 15 * 60_000 });

// When the editor shows that the object was removed:
const copy = recovery.copy(); // { uid, identity, draft, edits, removedAt } or null
if (copy) offerCopyOut(copy.edits);

// When the editor for this UID is torn down:
recovery.dispose();
```

`copy()` returns null while the object exists (edit the live draft instead), after `retainMs`, and
after `dispose()`. The copy is bound to the UID and identity it was made against: a recreated name
has a new UID and opens as a new editor, which never inherits the old draft. The copy is for
explicit copy-out only. It is never reconciled or applied automatically, so the store remains the
only place drafts are edited.

## Keep the local value in a conflict

`store.revert(uid, path)` resolves a conflict in the server's favour. [`keepLocal`](keepLocal.ts)
resolves it in the person's favour:

```ts
for (const { path } of store.conflicts(uid)) {
  if (choseLocal(path)) keepLocal(store, uid, path); // false when the path is no longer editable
}
const intent = store.captureSave(uid); // a fresh intent, bound to the version just reviewed
```

It captures the local value (or its absence), lets `revert` adopt the server's value and clear the
conflict, then reapplies the local value in the same synchronous call. The result is an ordinary
edit against the newer server version. Deletions stay deleted, a value the server deleted comes
back, arrays are kept whole, and deciding at a parent path resolves the conflicts beneath it. Other
edits and conflicts are untouched. A path that a policy or a later redaction makes read-only is
refused with `false`, and nothing changes. A value the server deleted is re-added as the last key
of its map, so a form that renders rows in key order may move that row.
