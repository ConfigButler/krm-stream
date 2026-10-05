import {
  type KRMObject,
  type LiveResourceStore,
  type NativeScope,
  nativeObjectURL,
} from "../../packages/krm-stream/src/index.ts";

export type SaveOutcome =
  | "unchanged"
  | "busy"
  | "saved"
  | "confirmed"
  | "draft-conflict"
  | "version-stale"
  | "recovering"
  | "unavailable";

/** A write or read the proxy refused, with the Kubernetes Status it answered: a 422 keeps its field
 * causes for the form, a 403 its reason. */
export class NativeRequestError extends Error {
  readonly httpStatus: number;
  readonly status: unknown;

  constructor(httpStatus: number, status: unknown) {
    const message = isRecord(status) && typeof status.message === "string" ? status.message : `HTTP ${httpStatus}`;
    super(`native request refused: ${message}`);
    this.httpStatus = httpStatus;
    this.status = status;
  }
}

/** Copy into the host. Edits one object of a native watch through the host proxy that watch reads.
 *
 * Pass the same `proxy` and `scope` given to nativeCollectionURL for the store's connectNativeWatch:
 * the editor reads and writes the object at that collection under its own namespace and name, so
 * its writes and recovery reads come from the source its draft was built from. request owns
 * session/CSRF policy; isLive reads that connection's state. */
export function nativeEditor(
  store: LiveResourceStore,
  uid: string,
  source: { proxy: string; scope: NativeScope },
  request: typeof fetch,
  isLive: () => boolean,
) {
  const { namespace, name } = store.server(uid).metadata;
  const url = nativeObjectURL(source.proxy, { ...source.scope, namespace, name });
  let saving = false;
  // The guarded read owed before the next write: "stale" after a version rejection, "confirm" after
  // a write whose outcome is unknown.
  let owed: "stale" | "confirm" | undefined;
  // The version an accepted write was based on, until its echo or a confirming read moves past it.
  let written: string | undefined;

  /** The read owed before the next write, if any. An accepted write is confirmed once the store
   * holds any later version: the write was conditional on `written`, so the next version is the
   * write itself and every one after it follows it. */
  function due(): "stale" | "confirm" | undefined {
    if (written !== undefined && store.server(uid).metadata.resourceVersion !== written) written = undefined;
    return owed ?? (written === undefined ? undefined : "confirm");
  }

  async function read(reason: "stale" | "confirm"): Promise<SaveOutcome> {
    owed = reason;
    const reconcile = store.captureReconciliation(uid);
    const latest = await request(url, { cache: "no-store", headers: { Accept: "application/json" } });
    if (latest.status === 404) return "unavailable";
    if (!latest.ok) throw new NativeRequestError(latest.status, await statusOf(latest));
    const object: unknown = await latest.json();
    // A native read answers with the object itself. Anything else is not this source.
    if (!isRecord(object) || !isRecord(object.metadata)) throw new Error("native read: not a Kubernetes object");
    if (!store.ids().includes(uid) || object.metadata.uid !== uid) return "unavailable";
    // No redaction metadata: a native object withholds nothing.
    const accepted = reconcile(object as KRMObject);
    // false has several causes. Do not infer readiness from it or force a refused response.
    // A later Save attempts only another guarded read until one is accepted while live.
    if (!isLive()) return "recovering";
    if (accepted) {
      // The base now holds the server's current state: whatever the write did, or did not do, is
      // visible as the remaining dirty fields.
      owed = undefined;
      written = undefined;
    }
    if (store.conflicts(uid).length) return "draft-conflict";
    if (!accepted) return "recovering";
    return reason === "stale" ? "version-stale" : "confirmed";
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
        const owedRead = due();
        if (owedRead) return await read(owedRead);
        const intent = store.captureSave(uid); // Patch, UID and base captured before any await.
        if (!intent) return "unchanged";
        // The preconditions travel inside the merge patch, where Kubernetes checks them atomically
        // with the write. The store never puts identity or version in a patch itself.
        const metadata = {
          ...(intent.patch.metadata as object),
          uid: intent.uid,
          resourceVersion: intent.resourceVersion,
        };
        // Until an answer says otherwise, the write may have landed: establish that by a read
        // before writing again, never by another write.
        owed = "confirm";
        const result = await request(url, {
          method: "PATCH",
          headers: { "Content-Type": "application/merge-patch+json", Accept: "application/json" },
          body: JSON.stringify({ ...intent.patch, metadata }),
        });
        if (result.ok) {
          owed = undefined;
          written = intent.resourceVersion;
          // The response is the written object. The watch delivers it; adopting it here could
          // overtake a newer event, so it is not read at all. Its echo, or confirm(), settles it.
          await result.body?.cancel().catch(() => {});
          return "saved";
        }
        // A 4xx is Kubernetes or the host refusing the write: it did not happen. A 5xx — the
        // proxy's 502 included — leaves the outcome unknown, and the read stays owed.
        if (result.status < 500) owed = undefined;
        // Deleted since its last event: the watch will remove it too.
        if (result.status === 404) return "unavailable";
        const status = await statusOf(result);
        // Kubernetes checks the version before the UID, so a stale version and an object replaced
        // under the same name both answer 409. A 422 on metadata.uid is the identity precondition.
        if (result.status === 409 || (result.status === 422 && namesUID(status))) {
          if (!store.ids().includes(uid)) return "unavailable";
          return await read("stale");
        }
        throw new NativeRequestError(result.status, status);
      } finally {
        saving = false;
      }
    },
    /** Read the object back through a guarded read, without writing: after a saved write whose
     * echo has not arrived within the host's patience, or to settle an unknown outcome. `confirmed`
     * means the draft's base is the server's current state; fields still dirty were not written. */
    async confirm(): Promise<SaveOutcome> {
      if (saving) return "busy";
      if (!store.ids().includes(uid)) return "unavailable";
      if (!isLive()) return "recovering";
      saving = true;
      try {
        return await read(owed ?? "confirm");
      } finally {
        saving = false;
      }
    },
  };
}

/** The Kubernetes Status a refusal carries, or undefined. */
async function statusOf(response: Response): Promise<unknown> {
  try {
    return await response.json();
  } catch {
    return undefined;
  }
}

function namesUID(status: unknown): boolean {
  const details = isRecord(status) ? status.details : undefined;
  const causes = isRecord(details) ? details.causes : undefined;
  return Array.isArray(causes) && causes.some((cause) => isRecord(cause) && cause.field === "metadata.uid");
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
