// The native transport: an ordinary Kubernetes LIST, then a WATCH from the collection's
// resourceVersion, through a proxy the host already runs. No gateway, no SSE, no projection: the
// objects are exactly what the proxy returns, and the host owns credentials, routing and
// authorization.
//
// A handle's first connection, and every connection after its checkpoint is discarded, starts with
// a fresh, complete LIST: the snapshot repairs whatever a previous watch missed — deletes, selector
// exits, a UID replaced under the same name — and the store prunes only once it completes. Once that
// snapshot is applied and its WATCH accepted, the handle keeps a CHECKPOINT: the resourceVersion of
// the last event its consumer applied, or of the last bookmark. An ordinary reconnect — EOF, a
// network failure, a retryable refusal — resumes the WATCH from it instead of listing again. The
// consumer already holds everything up to the checkpoint and the resumed watch delivers everything
// after it, in order: the same invariant any live watch with events in flight relies on. So a
// resumed watch starts no snapshot — no reset, no synced — and the store keeps its membership,
// drafts and conflicts. A write is protected against replayed staleness by its resourceVersion
// precondition, exactly as on a live watch.
//
// Expiry (410), malformed input and unclassifiable errors discard the checkpoint, and the next
// connection lists again. The checkpoint belongs to one connectNativeWatch call: it is never
// exported, accepted from a caller or shared between handles, so it is bound to that handle's URL,
// selectors and credentials. ResourceVersions are opaque: the checkpoint is replaced in stream
// order, never compared, parsed or ordered. Streaming lists and pagination are later work with their own
// recovery rules; this file deliberately implements neither.
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
const reserved = [
  "watch",
  "resourceVersion",
  "resourceVersionMatch",
  "limit",
  "continue",
  "sendInitialEvents",
  "allowWatchBookmarks",
];

/** Watch a native Kubernetes collection through a host proxy, delivering each state event to
 * `consume`. `collectionURL` is a collection path the proxy serves, such as one
 * nativeCollectionURL builds; its selectors apply to both the LIST and the WATCH.
 *
 * The first connection LISTs the complete collection and delivers `reset`, an `added` per member,
 * and — once the WATCH from the collection's resourceVersion is accepted — `synced`, after which the
 * state is `live` and watch events follow as `added`, `modified` and `deleted`. Every WATCH asks for
 * bookmarks; they are never delivered and change no object.
 *
 * After a complete snapshot the handle keeps a checkpoint: the resourceVersion of the last event the
 * consumer applied, or of the last bookmark. An EOF, a network failure, a truncated final frame or a
 * retryable refusal (408, 429 or 5xx, as an HTTP status or in the stream) RESUMES the WATCH from that
 * checkpoint: no LIST, no `reset` and no `synced`, and the state goes `connecting` → `live` once the
 * resumed watch is accepted. The consumer keeps everything it holds, drafts and conflicts included,
 * and receives what changed in the meantime — deletes and selector exits as `deleted`, a same-name
 * replacement as the old UID's `deleted` and the new UID's `added`.
 *
 * HTTP or in-stream 410 (history expired) is reported as RESYNC_REQUIRED and discards the
 * checkpoint, as do malformed frames and an in-stream error without a code: the next connection
 * LISTs again, and a store keeps its previous state until that snapshot completes and then prunes
 * what it no longer contains. An initialization interrupted before `synced` leaves no checkpoint
 * either. Every reconnect consumes the bounded retry budget, honoring `Retry-After` and a Status's
 * `retryAfterSeconds`. HTTP or in-stream 401, 403 and every other 4xx are terminal, on a resumed
 * watch too, as is a LIST response that is only one page of the collection. Host exceptions,
 * `close()` and `signal` behave exactly as for connectResourceStream.
 *
 * The checkpoint is private to the handle, so it is bound to this URL and these options: it is
 * never exposed, accepted or shared. Objects arrive as the proxy returns them, Secret values and
 * machinery fields included: native access provides no projection, redaction, suppression or watch
 * sharing. A source that refused a projected stream must never be replaced with this one, and
 * separate sources, scopes and identities need separate stores. */
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
  // This handle's own, for its whole life: no other handle, URL or identity ever reads it.
  const position: Position = { checkpoint: undefined, type: undefined };
  return runConnection((hooks, signal) => watchOnce(url, hooks, signal, opts, position), consume, opts);
}

/** The apiVersion and kind a typed collection's items have: `ConfigMapList` lists `ConfigMap`s. */
interface ItemType {
  apiVersion: string;
  kind: string;
}

/** What one handle carries from one connection to the next. Only that handle's transport reads or
 * writes it. */
interface Position {
  /** Where the next WATCH resumes: the resourceVersion of the last event the consumer applied, or of
   * the last bookmark, after a complete snapshot. Undefined when the next connection must LIST.
   * Opaque: replaced in stream order, never compared, parsed or ordered. */
  checkpoint: string | undefined;
  /** The item type the last complete LIST named, for watch objects without type metadata: a resumed
   * watch has no collection of its own to read it from. */
  type: ItemType | undefined;
}

/** A frame or a response this connector cannot use. Reported, never skipped: the connection ends,
 * the checkpoint is discarded, and the next connection re-lists. */
class Malformed extends Error {}

/** The watch ended inside a frame. Unlike malformed input this says nothing about what was
 * delivered: the partial frame was never applied, so the checkpoint still names the last event that
 * was, and the next connection resumes from it. */
class Truncated extends Error {}

/** Whether a refusal with this HTTP status, on the response or in the stream, leaves the checkpoint
 * usable: a transient condition of the server, not of the position. */
const resumable = (status: number) => status === 408 || status === 429 || status >= 500;

/** One connection: LIST, then WATCH until it ends — or, with a checkpoint, only the WATCH from it.
 * Resolves in every case, after the readers and listeners are released. */
async function watchOnce(
  url: string,
  hooks: TransportHooks,
  signal: AbortSignal,
  opts: ResourceStreamOptions,
  position: Position,
): Promise<void> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  const aborted = () => controller.signal.aborted;
  // Called detached, so a consumer never sees this hooks object as `this`.
  const { consume } = hooks;

  /** Report a refused response. The connection ends either way; only a transient refusal keeps the
   * checkpoint, so an expired or refused position is never asked for again. */
  const refused = async (phase: string, res: Response) => {
    if (!resumable(res.status)) position.checkpoint = undefined;
    const { code, terminal } = classify(res.status);
    const message = (await statusMessage(res, controller.signal)) ?? `native ${phase}: HTTP ${res.status}`;
    if (aborted()) return; // closed while reading the refusal: report nothing
    hooks.error(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
  };
  const malformed = (phase: string, error: Malformed) => {
    // Something was delivered that cannot be trusted, or something that should have been was not:
    // the position is unknown, and only a fresh snapshot repairs it.
    position.checkpoint = undefined;
    if (!aborted()) hooks.error("INTERNAL", `native ${phase}: ${error.message}`, false);
  };
  /** The consumer applied an event, or the stream passed a bookmark: the next connection resumes
   * after it. An event without a usable resourceVersion leaves no position to resume from. Nothing
   * moves once the stream is closing, which is also how a consumer exception leaves it unmoved. */
  const advance = (resourceVersion: unknown) => {
    if (aborted()) return;
    position.checkpoint = typeof resourceVersion === "string" && resourceVersion !== "" ? resourceVersion : undefined;
  };

  try {
    if (aborted()) return;
    // Read once: this connection resumes or re-lists, never both.
    const resumeFrom = position.checkpoint;
    let from: string;
    if (resumeFrom === undefined) {
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
      position.type = collection.type;
      from = collection.resourceVersion;

      // Membership is established only by a COMPLETE collection, so nothing above this line delivers.
      hooks.reset();
      if (aborted()) return;
      consume({ type: "reset" });
      for (const object of collection.items) {
        if (aborted()) return;
        consume({ type: "added", object });
      }
      if (aborted()) return;
    } else {
      from = resumeFrom;
    }

    // Bookmarks keep a quiet collection's checkpoint recent enough to resume from.
    const watched = await request(
      `${url}${url.includes("?") ? "&" : "?"}watch=1&allowWatchBookmarks=true&resourceVersion=${encodeURIComponent(from)}`,
      "application/json",
      controller.signal,
      opts,
    );
    if (!watched.ok || !watched.body) return await refused("watch", watched);
    if (aborted()) return void (await watched.body.cancel().catch(() => {}));
    if (resumeFrom === undefined) {
      // The snapshot is applied and the watch is accepted: only now is the state complete and live.
      consume({ type: "synced" });
      // A consumer that closed the stream — or threw — while applying `synced` gets no later `live`,
      // and an initialization that never completed leaves no checkpoint: the next connection lists.
      if (aborted()) return void (await watched.body.cancel().catch(() => {}));
      position.checkpoint = from;
    }
    // A resumed watch starts no snapshot: the consumer already holds everything up to the checkpoint
    // and this watch delivers everything after it, in order. Its acceptance alone makes it live.
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
          if (error instanceof Truncated) {
            // The checkpoint stands: it names the last event applied, and the partial one was not.
            return void hooks.error("INTERNAL", `native watch: ${error.message}`, false);
          }
          if (!(error instanceof Malformed)) throw error;
          return malformed("watch", error);
        }
        for (const line of lines) {
          // One chunk can hold many frames. Nothing after a close is delivered.
          if (aborted()) return;
          let more: boolean;
          try {
            more = deliver(line, position.type, hooks, advance, position);
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
    // The network failed. This connection is over, and the lifecycle decides about the next. The
    // checkpoint still names the last event applied, so the next connection resumes from it. Host
    // callbacks never land here: the lifecycle catches their exceptions itself.
  } finally {
    controller.abort();
    signal.removeEventListener("abort", abort);
  }
}

/** Apply one watch frame, then advance the checkpoint past it; false when the connection must end.
 * Throws Malformed. */
function deliver(
  line: string,
  type: ItemType | undefined,
  hooks: TransportHooks,
  advance: (resourceVersion: unknown) => void,
  position: Position,
): boolean {
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
    case "MODIFIED": {
      const applied = resource(object, type, "watch");
      hooks.consume({ type: frame.type === "ADDED" ? "added" : "modified", object: applied });
      advance(applied.metadata.resourceVersion);
      return true;
    }
    case "DELETED": {
      const removed = resource(object, type, "watch");
      hooks.consume({ type: "deleted", identity: identity(removed) });
      advance(removed.metadata.resourceVersion);
      return true;
    }
    case "BOOKMARK":
      // A collection checkpoint, not an object version: it changes neither membership nor any
      // object, and the consumer never sees it. It only moves where the next watch resumes.
      advance(isRecord(object.metadata) ? object.metadata.resourceVersion : undefined);
      return true;
    case "ERROR": {
      const { code, terminal, message, retryAfterMs, keepsCheckpoint } = statusError(object);
      if (!keepsCheckpoint) position.checkpoint = undefined;
      hooks.error(code, message, terminal, retryAfterMs);
      // The API server ends the watch after an error. A transient one resumes; any other re-lists.
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

/** A watch ERROR event's Kubernetes Status, classified like the HTTP status it carries. Only a coded
 * transient error keeps the checkpoint: an expiry names the position itself as the problem, and an
 * error without a code cannot be told apart from one, so it re-lists. */
function statusError(status: Record<string, unknown>) {
  const reason = typeof status.reason === "string" ? status.reason : undefined;
  const expired = reason === "Expired" || reason === "Gone";
  const code = typeof status.code === "number" ? status.code : expired ? 410 : undefined;
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
    keepsCheckpoint: code !== undefined && !expired && resumable(code),
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
 * complete. Throws Malformed on invalid UTF-8, and Truncated when the stream ends mid-frame — the two
 * are kept apart because only malformed input casts doubt on what was already delivered. */
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

  /** The stream ended. Anything still buffered — a character cut off included — is a truncated frame:
   * every complete line has already been returned, so the cut can only be inside the last one. */
  end(): void {
    let rest: string;
    try {
      rest = this.#utf8.decode();
    } catch {
      throw new Truncated("the watch ended inside a UTF-8 character");
    }
    if ((this.#buffer + rest).trim() !== "") throw new Truncated("the watch ended inside a frame");
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
