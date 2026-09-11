import { shallowRef, onScopeDispose } from "vue";
import type { LiveResourceStore, ManagedStreamHandle } from "../../packages/krm-stream/src/index.ts";

/** Call in setup() or an active effect scope. One fixed UID per instance.
 * The caller owns the connection; disposing one editor only removes its subscriptions. */
export function useLiveResource(store: LiveResourceStore, uid: string, connection: ManagedStreamHandle) {
  const read = () =>
    store.ids().includes(uid)
      ? {
          draft: store.draft(uid),
          changes: store.changes(uid),
          conflicts: store.conflicts(uid),
          redactions: store.redactions(uid),
        }
      : null;
  const resource = shallowRef(read());
  const state = shallowRef(connection.state);
  const stopStore = store.subscribe(() => {
    resource.value = read();
  });
  const stopConnection = connection.subscribe((next) => {
    state.value = next;
  });
  onScopeDispose(() => {
    stopStore();
    stopConnection();
  });
  return { resource, state };
}
