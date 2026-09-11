import type { KRMObject, LiveResourceStore, Redaction } from "../../packages/krm-stream/src/index.ts";

/** Copy into the host. request is the host's fetch wrapper (session/CSRF/error policy).
 * Call save only while the managed connection is live. Keep the editor mounted during requests. */
export function conditionalEditor(store: LiveResourceStore, uid: string, url: string, request: typeof fetch) {
  let saving = false;
  return {
    get saving() {
      return saving;
    },
    async save(): Promise<"unchanged" | "busy" | "saved" | "conflict"> {
      if (saving) return "busy";
      if (store.conflicts(uid).length) return "conflict";
      const intent = store.captureSave(uid); // No await between patch and merge-base capture.
      if (!intent) return "unchanged";
      saving = true;
      try {
        const result = await request(url, {
          method: "PATCH",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(intent),
        });
        if (result.status === 409) {
          // Do not clear the draft or blindly retry the old patch using a newer version.
          // A watch may already have deleted the object; don't resurrect it through a GET.
          if (!store.ids().includes(uid)) return "conflict";
          const reconcile = store.captureReconciliation(uid);
          const latest = await request(url, { cache: "no-store" });
          if (latest.status === 404) return "conflict"; // Let the stream remove the old UID.
          if (!latest.ok) throw new Error(`Reconciliation failed: HTTP ${latest.status}`);
          const body = (await latest.json()) as { object: KRMObject; redacted: Redaction[] | null };
          reconcile(body.object, { redacted: body.redacted ?? [] });
          // A newer watch/GET wins if reconcile returns false. Render the current store either way.
          // Ask the user to review conflicts and save again; capture a fresh intent on that click.
          return "conflict";
        }
        if (!result.ok) throw new Error(`Save failed: HTTP ${result.status}`);
        // No adoptSaved/clear/reset: even edits made while saving survive the watch echo.
        return "saved";
      } finally {
        saving = false;
      }
    },
  };
}
