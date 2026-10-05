// The native transport: an ordinary Kubernetes LIST, then a WATCH from the collection's
// resourceVersion, through a proxy the host already runs. No gateway, no SSE, no projection: the
// objects are exactly what the proxy returns, and the host owns credentials, routing and
// authorization.
//
// Every connection starts with a fresh, complete LIST, so a reconnect needs no checkpoint: the
// snapshot repairs whatever the previous watch missed — deletes, selector exits, a UID replaced
// under the same name — and the store prunes only once it completes. Resumable watches, streaming
// lists and pagination are later work with their own recovery rules; this file deliberately
// implements none of them.
//
// lifecycle.ts owns retries, state and cancellation, shared with the gateway connector. This file
// owns framing and its classification, and imports nothing from the SSE side: there is no `seq`, no
// protocol header and no redaction here.

import { refusal, request, retryAfter, statusMessage } from "./http.ts";
import {
  type ResourceEventConsumer,
  type ResourceStreamHandle,
  type ResourceStreamOptions,
  runConnection,
  type TransportHooks,
} from "./lifecycle.ts";
import type { ErrorCode, Identity, KRMObject } from "./types.ts";

/** Query parameters the connector sets itself, refused in a caller's collection URL. */
const reserved = ["watch", "resourceVersion", "resourceVersionMatch", "limit", "continue", "sendInitialEvents"];

/** Watch a native Kubernetes collection through a host proxy, delivering each state event to
 * `consume`. `collectionURL` is a collection path the proxy serves, such as one
 * nativeCollectionURL builds; its selectors apply to both the LIST and the WATCH.
 *
 * Each connection LISTs the complete collection and delivers `reset`, an `added` per member, and —
 * once the WATCH from the collection's resourceVersion is accepted — `synced`, after which the state
 * is `live` and watch events follow as `added`, `modified` and `deleted`. Bookmarks change nothing.
 * Every reconnect LISTs again, so a store keeps its previous state until the next snapshot completes
 * and then prunes what it no longer contains.
 *
 * HTTP or in-stream 410 (history expired), 408, 429, 5xx, network failures, EOF and malformed or
 * truncated frames consume the bounded retry budget, honoring `Retry-After` and a Status's
 * `retryAfterSeconds`. HTTP or in-stream 401, 403 and every other 4xx are terminal, as is a LIST
 * response that is only one page of the collection. Host exceptions, `close()` and `signal` behave
 * exactly as for connectResourceStream.
 *
 * Objects arrive as the proxy returns them, Secret values and machinery fields included: native
 * access provides no projection, redaction, suppression or watch sharing. A source that refused a
 * projected stream must never be replaced with this one, and separate sources, scopes and identities
 * need separate stores. */
export function connectNativeWatch(
  collectionURL: string,
  consume: ResourceEventConsumer,
  opts: ResourceStreamOptions = {},
): ResourceStreamHandle {
  const url = collectionURL.split("#")[0]!;
  const query = new URLSearchParams(url.includes("?") ? url.slice(url.indexOf("?") + 1) : "");
  for (const name of reserved) {
    if (query.has(name)) throw new Error(`krm-stream: the collection URL must not set ${name}; the connector does`);
  }
  return runConnection((hooks, signal) => watchOnce(url, hooks, signal, opts), consume, opts);
}

/** The apiVersion and kind a typed collection's items have: `ConfigMapList` lists `ConfigMap`s. */
interface ItemType {
  apiVersion: string;
  kind: string;
}

/** A frame or a response this connector cannot use. Reported, never skipped: the connection ends
 * and the next one re-lists. */
class Malformed extends Error {}

/** One connection: LIST, then WATCH until it ends. Resolves in every case, after the readers and
 * listeners are released. */
async function watchOnce(
  url: string,
  hooks: TransportHooks,
  signal: AbortSignal,
  opts: ResourceStreamOptions,
): Promise<void> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  const aborted = () => controller.signal.aborted;
  // Called detached, so a consumer never sees this hooks object as `this`.
  const { consume } = hooks;

  /** Report a refused response. The connection ends either way. */
  const refused = async (phase: string, res: Response) => {
    const { code, terminal } = classify(res.status);
    const message = (await statusMessage(res, controller.signal)) ?? `native ${phase}: HTTP ${res.status}`;
    if (aborted()) return; // closed while reading the refusal: report nothing
    hooks.error(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
  };
  const malformed = (phase: string, error: Malformed) => {
    if (!aborted()) hooks.error("INTERNAL", `native ${phase}: ${error.message}`, false);
  };

  try {
    if (aborted()) return;
    const listed = await request(url, "application/json", controller.signal, opts);
    if (!listed.ok || !listed.body) return await refused("list", listed);
    if (aborted()) return void (await listed.body.cancel().catch(() => {}));
    hooks.opened();

    let collection: Collection;
    try {
      const text = await readText(listed.body, controller.signal);
      if (text === undefined) return;
      collection = readCollection(text);
    } catch (error) {
      if (error instanceof Unpaginated) {
        if (!aborted()) hooks.error("INTERNAL", error.message, true);
        return;
      }
      if (error instanceof Malformed) return malformed("list", error);
      throw error; // the network failed while the body arrived
    }

    // Membership is established only by a COMPLETE collection, so nothing above this line delivers.
    hooks.reset();
    if (aborted()) return;
    consume({ type: "reset" });
    for (const object of collection.items) {
      if (aborted()) return;
      consume({ type: "added", object });
    }
    if (aborted()) return;

    const watched = await request(
      `${url}${url.includes("?") ? "&" : "?"}watch=1&resourceVersion=${encodeURIComponent(collection.resourceVersion)}`,
      "application/json",
      controller.signal,
      opts,
    );
    if (!watched.ok || !watched.body) return await refused("watch", watched);
    if (aborted()) return void (await watched.body.cancel().catch(() => {}));
    // The snapshot is applied and the watch is accepted: only now is the state complete and live.
    consume({ type: "synced" });
    // A consumer that closed the stream — or threw — while applying `synced` gets no later `live`.
    if (aborted()) return void (await watched.body.cancel().catch(() => {}));
    hooks.synced();

    const reader = watched.body.getReader();
    const cancelReader = () => {
      void reader.cancel().catch(() => {});
    };
    controller.signal.addEventListener("abort", cancelReader, { once: true });
    if (aborted()) cancelReader();
    const decoder = new WatchDecoder();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (aborted()) return;
        let lines: string[];
        try {
          if (done) return decoder.end();
          lines = decoder.push(value);
        } catch (error) {
          if (!(error instanceof Malformed)) throw error;
          return malformed("watch", error);
        }
        for (const line of lines) {
          // One chunk can hold many frames. Nothing after a close is delivered.
          if (aborted()) return;
          let more: boolean;
          try {
            more = deliver(line, collection.type, hooks);
          } catch (error) {
            if (!(error instanceof Malformed)) throw error;
            return malformed("watch", error);
          }
          if (!more) return;
        }
      }
    } finally {
      controller.signal.removeEventListener("abort", cancelReader);
      await reader.cancel().catch(() => {});
    }
  } catch {
    // The network failed. This connection is over, and the lifecycle decides about the next. Host
    // callbacks never land here: the lifecycle catches their exceptions itself.
  } finally {
    controller.abort();
    signal.removeEventListener("abort", abort);
  }
}

/** Apply one watch frame; false when the connection must end. Throws Malformed. */
function deliver(line: string, type: ItemType | undefined, hooks: TransportHooks): boolean {
  let frame: unknown;
  try {
    frame = JSON.parse(line);
  } catch {
    throw new Malformed("a watch frame is not JSON");
  }
  if (!isRecord(frame) || typeof frame.type !== "string" || !isRecord(frame.object)) {
    throw new Malformed("a watch frame has no type or object");
  }
  const object = frame.object;
  switch (frame.type) {
    case "ADDED":
      hooks.consume({ type: "added", object: resource(object, type, "watch") });
      return true;
    case "MODIFIED":
      hooks.consume({ type: "modified", object: resource(object, type, "watch") });
      return true;
    case "DELETED":
      hooks.consume({ type: "deleted", identity: identity(resource(object, type, "watch")) });
      return true;
    case "BOOKMARK":
      // A collection checkpoint, not an object version: it changes neither membership nor any
      // object. Every reconnect re-lists, so there is nothing to resume from it either.
      return true;
    case "ERROR": {
      const { code, terminal, message, retryAfterMs } = statusError(object);
      hooks.error(code, message, terminal, retryAfterMs);
      // The API server ends the watch after an error; either way, the next connection re-lists.
      return false;
    }
    default:
      throw new Malformed(`unknown watch event type ${JSON.stringify(frame.type)}`);
  }
}

/** How a native HTTP status or Status code is reported. Unlike the gateway's HTTP 410, a native 410
 * means the watch history expired, which a fresh LIST recovers. */
function classify(status: number): { code: ErrorCode; terminal: boolean } {
  return status === 410 ? { code: "RESYNC_REQUIRED", terminal: false } : refusal(status);
}

/** A watch ERROR event's Kubernetes Status, classified like the HTTP status it carries. */
function statusError(status: Record<string, unknown>) {
  const reason = typeof status.reason === "string" ? status.reason : undefined;
  const code =
    typeof status.code === "number" ? status.code : reason === "Expired" || reason === "Gone" ? 410 : undefined;
  const { code: errorCode, terminal } =
    code === undefined ? { code: "INTERNAL" as const, terminal: false } : classify(code);
  const text = typeof status.message === "string" ? status.message : "";
  const details = status.details;
  const seconds = isRecord(details) ? details.retryAfterSeconds : undefined;
  return {
    code: errorCode,
    terminal,
    message: text !== "" ? text : `native watch: error ${code ?? reason ?? "without a status"}`,
    retryAfterMs: typeof seconds === "number" && seconds >= 0 ? seconds * 1000 : undefined,
  };
}

interface Collection {
  items: KRMObject[];
  resourceVersion: string;
  type: ItemType | undefined;
}

/** A LIST response that is one page of a larger collection. Pruning from it would delete every
 * object on the other pages, so it is refused rather than retried. */
class Unpaginated extends Error {}

/** Parse and validate a complete collection. Throws Unpaginated or Malformed. */
function readCollection(text: string): Collection {
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new Malformed("the collection is not JSON");
  }
  if (!isRecord(body)) throw new Malformed("the collection is not an object");
  const metadata = isRecord(body.metadata) ? body.metadata : {};
  if (typeof metadata.continue === "string" && metadata.continue !== "") {
    throw new Unpaginated(
      "native list: the response is one page of a paginated collection, which this connector does not support",
    );
  }
  const resourceVersion = metadata.resourceVersion;
  if (typeof resourceVersion !== "string" || resourceVersion === "") {
    throw new Malformed("the collection has no metadata.resourceVersion");
  }
  const raw = body.items;
  if (raw !== null && !Array.isArray(raw)) throw new Malformed("the collection has no items array");
  const type = itemType(body);
  const uids = new Set<string>();
  const items = (raw ?? []).map((item: unknown) => {
    const object = resource(item, type, "list");
    if (uids.has(object.metadata.uid)) throw new Malformed(`the collection lists UID ${object.metadata.uid} twice`);
    uids.add(object.metadata.uid);
    return object;
  });
  return { items, resourceVersion, type };
}

/** The item type of a concrete typed collection, or undefined. Only a `<Kind>List` names its items'
 * kind; a generic `List`, a `Table` or a `meta.k8s.io` collection does not, and a kind is never
 * guessed from a plural resource name. */
function itemType(collection: Record<string, unknown>): ItemType | undefined {
  const { apiVersion, kind } = collection;
  if (typeof apiVersion !== "string" || apiVersion === "" || apiVersion.startsWith("meta.k8s.io/")) return undefined;
  if (typeof kind !== "string" || !/^[A-Za-z0-9]+List$/.test(kind)) return undefined;
  return { apiVersion, kind: kind.slice(0, -"List".length) };
}

/** A complete object with the identity a store keys on. Type metadata already present is kept;
 * missing type metadata comes from the collection's item type. Throws Malformed. */
function resource(value: unknown, type: ItemType | undefined, phase: "list" | "watch"): KRMObject {
  const where = phase === "list" ? "a collection item" : "a watch object";
  if (!isRecord(value) || !isRecord(value.metadata)) throw new Malformed(`${where} has no metadata`);
  const { uid, name, namespace } = value.metadata;
  if (typeof uid !== "string" || uid === "") throw new Malformed(`${where} has no metadata.uid`);
  if (typeof name !== "string" || name === "") throw new Malformed(`${where} ${uid} has no metadata.name`);
  if (namespace !== undefined && typeof namespace !== "string") {
    throw new Malformed(`${where} ${uid} has an invalid metadata.namespace`);
  }
  const present = (field: unknown) => typeof field === "string" && field !== "";
  if (present(value.apiVersion) && present(value.kind)) return value as KRMObject;
  const apiVersion = present(value.apiVersion) ? value.apiVersion : type?.apiVersion;
  const kind = present(value.kind) ? value.kind : type?.kind;
  if (apiVersion === undefined || kind === undefined) {
    throw new Malformed(`${where} ${uid} has no apiVersion or kind, and the collection does not name its item type`);
  }
  return { ...value, apiVersion, kind } as KRMObject;
}

function identity(object: KRMObject): Identity {
  const { uid, name, namespace } = object.metadata;
  return {
    uid,
    apiVersion: object.apiVersion,
    kind: object.kind,
    ...(namespace === undefined ? {} : { namespace }),
    name,
  };
}

/** Incremental decoder for native watch JSON: one event per line. Bytes arrive in arbitrary chunks,
 * so a frame — or a multi-byte character — can be split anywhere; this buffers until a line is
 * complete. Throws Malformed on invalid UTF-8 and when the stream ends mid-frame. */
export class WatchDecoder {
  #utf8 = new TextDecoder("utf-8", { fatal: true });
  #buffer = "";

  /** Feed a chunk; get back the complete, nonblank lines it finished. */
  push(chunk: Uint8Array): string[] {
    this.#buffer += decode(this.#utf8, chunk);
    const lines: string[] = [];
    for (;;) {
      const end = this.#buffer.indexOf("\n");
      if (end === -1) break;
      const line = this.#buffer.slice(0, end).trim();
      this.#buffer = this.#buffer.slice(end + 1);
      if (line !== "") lines.push(line);
    }
    return lines;
  }

  /** The stream ended. Anything still buffered is a truncated frame. */
  end(): void {
    this.#buffer += decode(this.#utf8);
    if (this.#buffer.trim() !== "") throw new Malformed("the watch ended inside a frame");
  }
}

/** A response body as text, or undefined once `signal` aborts. The body is always released. Throws
 * Malformed on invalid UTF-8. */
async function readText(body: ReadableStream<Uint8Array>, signal: AbortSignal): Promise<string | undefined> {
  const reader = body.getReader();
  const cancel = () => {
    void reader.cancel().catch(() => {});
  };
  signal.addEventListener("abort", cancel, { once: true });
  if (signal.aborted) cancel();
  const utf8 = new TextDecoder("utf-8", { fatal: true });
  let text = "";
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (signal.aborted) return undefined;
      if (done) return text + decode(utf8);
      text += decode(utf8, value);
    }
  } finally {
    signal.removeEventListener("abort", cancel);
    await reader.cancel().catch(() => {});
  }
}

/** Decode a chunk, or flush the decoder when there is none. Invalid or incomplete UTF-8 is malformed
 * input, never a replacement character in a resource. */
function decode(utf8: TextDecoder, chunk?: Uint8Array): string {
  try {
    return chunk === undefined ? utf8.decode() : utf8.decode(chunk, { stream: true });
  } catch {
    throw new Malformed("the response is not valid UTF-8");
  }
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
