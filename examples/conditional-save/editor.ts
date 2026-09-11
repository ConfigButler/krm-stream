import type { KRMObject, LiveResourceStore } from "../../packages/krm-stream/src/index.ts";

export type SaveOutcome =
  | "unchanged"
  | "busy"
  | "saved"
  | "draft-conflict"
  | "version-stale"
  | "recovering"
  | "unavailable";

/** Copy into the host. request owns session/CSRF/error policy; isLive reads managed connection state. */
export function conditionalEditor(
  store: LiveResourceStore,
  uid: string,
  url: string,
  request: typeof fetch,
  isLive: () => boolean,
) {
  let saving = false;
  let needsRead = false;
  async function refresh(): Promise<SaveOutcome> {
    needsRead = true;
    const reconcile = store.captureReconciliation(uid);
    const latest = await request(url, { cache: "no-store" });
    if (latest.status === 404) return "unavailable";
    if (!latest.ok) throw new Error(`Reconciliation failed: HTTP ${latest.status}`);
    const body = (await latest.json()) as { object: KRMObject; redactedPaths: string[] };
    if (!store.ids().includes(uid) || body.object.metadata.uid !== uid) return "unavailable";
    const accepted = reconcile(body.object, { redactedPaths: body.redactedPaths });
    // false has several causes. Do not infer readiness from it or force a refused response.
    // A later Save attempts only another guarded read until one is accepted while live.
    if (!isLive()) return "recovering";
    needsRead = !accepted;
    if (store.conflicts(uid).length) return "draft-conflict";
    return accepted ? "version-stale" : "recovering";
  }
  return {
    get saving() {
      return saving;
    },
    async save(): Promise<SaveOutcome> {
      if (saving) return "busy";
      if (!store.ids().includes(uid)) return "unavailable";
      if (!isLive()) return "recovering";
      if (store.conflicts(uid).length) return "draft-conflict";
      saving = true;
      try {
        if (needsRead) return await refresh();
        const intent = store.captureSave(uid); // Patch, UID and base captured before any await.
        if (!intent) return "unchanged";
        const result = await request(url, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(intent),
        });
        if (result.status === 409) {
          if (!store.ids().includes(uid)) return "unavailable";
          return await refresh();
        }
        if (!result.ok) throw new Error(`Save failed: HTTP ${result.status}`);
        // Never adopt a save response or clear drafts: edits made during the write must survive.
        return "saved";
      } finally {
        saving = false;
      }
    },
  };
}
