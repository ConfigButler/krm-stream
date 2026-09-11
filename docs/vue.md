# Vue integration

A copyable Vue 3 composable covers the adapter: reactive snapshots for drafts, changes, conflicts,
redactions and connection state, plus automatic subscription cleanup. Vue remains a host dependency;
the core package and its browser bundle have no framework dependencies.

The [composable source](../examples/vue/useLiveResource.ts) and
[tests](../examples/vue/useLiveResource.test.ts) are typechecked and executed by `task test-vue` and CI.
Copy the source into your host and change its library import to `@configbutler/krm-stream`.

```ts
const { resource, state } = useLiveResource(store, uid, connection);
```

Use `resource.value?.draft` in script and `resource?.draft` in a template. A missing/deleted UID gives
`null`. For a changing selection, remount a keyed editor or create a new effect scope. Use store
methods such as `setValue`, `removeKey` and conflict resolution methods for edits: do not bind
`v-model` directly to the draft snapshot. Store reads are detached copies, and editing them bypasses
policy checks and change notifications.

The connection belongs to its creator. A parent sharing one connection across many editors closes
it when the parent unmounts; an editor that creates its own connection can register
`onScopeDispose(() => connection.close())`. Removing one editor's subscriptions must not close a
connection used by other editors. Capture and submit saves with the
[conditional-save example](../examples/conditional-save/README.md); expose request errors and saving
state from that host-owned controller. Enable Save while `state.status === "live"`.
