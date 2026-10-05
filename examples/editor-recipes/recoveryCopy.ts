import type { Change, KRMObject, LiveResourceStore } from "../../packages/krm-stream/src/index.ts";

/** What a recovery copy holds: the person's draft and unsaved edits, as they last stood before the
 * object left the store, bound to the identity they were made against. */
export interface RecoveryCopy {
  uid: string;
  identity: { apiVersion: string; kind: string; namespace?: string; name: string };
  draft: KRMObject;
  edits: Change[];
  /** When the object left the store: a `deleted` event or a snapshot that no longer sent it. */
  removedAt: number;
}

export interface RecoveryOptions {
  /** How long a removed object's copy stays available. Host policy; defaults to 15 minutes. */
  retainMs?: number;
  /** Injectable clock for tests. */
  now?: () => number;
}

/** Copy into the host. Keeps a detached copy of one fixed UID's draft while it exists, so the
 * person's work can be copied out after a deletion or snapshot pruning removes the object — and its
 * draft — from the store. The store cannot help after the fact: once the UID is gone there is no
 * draft left to read, so the copy is refreshed on every notification while it exists.
 *
 * The copy is for explicit copy-out only. It is never applied automatically, never reconciled, and
 * never offered to a replacement object: a recreated name has a new UID and opens as a new editor.
 * One store stays the only place drafts are edited. */
export function retainRecoveryCopy(store: LiveResourceStore, uid: string, options: RecoveryOptions = {}) {
  const retainMs = options.retainMs ?? 15 * 60_000;
  const now = options.now ?? Date.now;
  let last: Omit<RecoveryCopy, "removedAt"> | null = null;
  let removedAt: number | undefined;
  let disposed = false;

  const capture = () => {
    if (disposed) return;
    if (!store.ids().includes(uid)) {
      // Removed (or never seen). Keep the last copy; there is no draft left to read.
      if (last && removedAt === undefined) removedAt = now();
      return;
    }
    const draft = store.draft(uid);
    const { apiVersion, kind, metadata } = draft;
    last = {
      uid,
      identity: { apiVersion, kind, name: metadata.name, ...(metadata.namespace ? { namespace: metadata.namespace } : {}) },
      draft,
      edits: store.changes(uid),
    };
  };
  capture(); // the initial state counts: an object deleted before any further notification is still recoverable
  const stop = store.subscribe(capture);

  return {
    // Both readers capture first: a host listener subscribed before this recipe runs before its
    // listener, and would otherwise read a removal the copy has not yet seen.
    /** The live object's draft is the one to edit; a copy exists only once the object is gone. */
    get removed(): boolean {
      capture();
      return removedAt !== undefined;
    },
    /** The retained copy of a removed object, or null while it still exists, after expiry, after
     * dispose(), or when it was never in the store. Each call returns a fresh detached copy. */
    copy(): RecoveryCopy | null {
      capture();
      if (!last || removedAt === undefined || now() - removedAt > retainMs) return null;
      return structuredClone({ ...last, removedAt });
    },
    /** Stop listening and drop the copy. Call when the editor for this UID is torn down. */
    dispose(): void {
      disposed = true;
      stop();
      last = null;
    },
  };
}
