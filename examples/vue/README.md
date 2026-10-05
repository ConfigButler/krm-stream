# Vue adapter example

A copyable Vue 3 composable covers the adapter: reactive snapshots for drafts, changes, conflicts,
redactions and connection state, plus automatic subscription cleanup. Vue remains a host dependency;
the core package and its browser bundle have no framework dependencies.

The [composable source](useLiveResource.ts) and
[tests](useLiveResource.test.ts) are typechecked and executed by `task test-vue` and CI.
Copy the source into your host and change its library import to `@configbutler/krm-stream`.

```ts
const { resource, state } = useLiveResource(store, uid, connection);
```

Use `resource.value?.draft` in script and `resource?.draft` in a template. A missing/deleted UID gives
`null`. For deletion recovery, capture the initial draft and subscribe to store notifications, retaining
`store.draft(uid)` while the fixed UID exists and keeping the last copy once it is absent. Capturing
only when Save is clicked loses subsequent edits. The null notification is too late to read the
removed draft. Use the tested
[recovery-copy recipe](../editor-recipes/README.md#recover-work-after-a-deletion) beside
`useLiveResource` for the same UID, dispose it with the editor, and never apply a recovery copy
automatically to a replacement UID.

For a changing selection, remount a keyed editor or create a new effect scope. Use store
methods such as `setValue`, `removeKey`, `revert` and the
[keep-local recipe](../editor-recipes/README.md#keep-the-local-value-in-a-conflict) for
edits: do not bind `v-model` directly to the draft snapshot. Store reads are detached copies, and
editing them bypasses policy checks and change notifications.

The connection belongs to its creator. A parent sharing one connection across many editors closes
it when the parent unmounts; an editor that creates its own connection can register
`onScopeDispose(() => connection.close())`. Removing one editor's subscriptions must not close a
connection used by other editors. Capture and submit saves with the
[conditional-save example](../conditional-save/README.md); expose request errors and saving
state from that host-owned controller. Enable Save while `state.status === "live"`.

Vue is a dependency of this private example only; the core package and bundle remain framework-independent.
