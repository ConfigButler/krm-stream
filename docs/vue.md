# Vue integration

A copyable Vue 3 composable covers the adapter: reactive snapshots for drafts, changes, conflicts,
redactions and connection state, plus automatic subscription cleanup. Vue remains a host dependency;
the core package and its browser bundle have no framework dependencies.

```ts
import { shallowRef, onScopeDispose } from "vue";
import type { LiveResourceStore, ManagedStreamHandle } from "@configbutler/krm-stream";

// Call in setup() or an active effect scope. One fixed UID per instance.
export function useLiveResource(
  store: LiveResourceStore,
  uid: string,
  connection: ManagedStreamHandle,
) {
  const read = () => store.ids().includes(uid) ? {
    draft: store.draft(uid),
    changes: store.changes(uid),
    conflicts: store.conflicts(uid),
    redactions: store.redactions(uid),
  } : null;
  const resource = shallowRef(read());
  const state = shallowRef(connection.state);
  const stopStore = store.subscribe(() => { resource.value = read(); });
  const stopConnection = connection.subscribe(next => { state.value = next; });
  onScopeDispose(() => { stopStore(); stopConnection(); });
  return { resource, state };
}
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
