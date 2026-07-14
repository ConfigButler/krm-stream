// The transport, consumer side. Everything else in this package is pure logic; this is the only file
// that knows a stream is made of bytes.
//
// Two ways in, because there are two authentication stories (spec §7) and neither is optional:
//
//   connectResourceStream    fetch-based. Needed for `Authorization: Bearer`, because native
//                            EventSource cannot send a custom header. Works in Node, so it is what
//                            the conformance suite drives.
//   connectWithEventSource   native EventSource. Needed for the same-origin session-cookie case,
//                            which is the BASELINE a v1 gateway must support.
//
// The rule both must obey, and the one that is easy to get wrong: on a TERMINAL error, close the
// connection. EventSource reconnects automatically otherwise, and will hammer a scope it can never
// be allowed to see — forever, from every open tab.

import type { LiveResourceStore } from "./store.ts";
import type { ErrorCode, Path, StreamEvent } from "./types.ts";

/** Incremental SSE parser. Bytes arrive in whatever chunks the network feels like — a frame can be
 * split down the middle, and it WILL be, under exactly the load where you least want to debug it —
 * so this buffers and only yields complete frames. */
export class SSEDecoder {
  #buffer = "";

  /** Feed a chunk of the stream; get back the events that completed with it. */
  push(chunk: string): StreamEvent[] {
    this.#buffer += chunk;
    const out: StreamEvent[] = [];

    // Frames are separated by a blank line. Normalize completed line endings first: an SSE line may
    // end \n, \r\n or bare \r, but a trailing \r may be the first byte of a split \r\n pair.
    const trailingCR = this.#buffer.endsWith("\r");
    const complete = trailingCR ? this.#buffer.slice(0, -1) : this.#buffer;
    this.#buffer = complete.replace(/\r\n|\r/g, "\n") + (trailingCR ? "\r" : "");

    for (;;) {
      const sep = this.#buffer.indexOf("\n\n");
      if (sep === -1) break; // an incomplete frame stays in the buffer until the rest of it arrives
      const frame = this.#buffer.slice(0, sep);
      this.#buffer = this.#buffer.slice(sep + 2);
      const ev = parseFrame(frame);
      if (ev) out.push(ev);
    }
    return out;
  }
}

/** Tracks the mandatory per-connection event sequence. The first missing frame is enough to make
 * state uncertain, so transports close and reconnect rather than applying a possibly stale tail. */
export class StreamSequence {
  #next = 1;

  observe(event: StreamEvent): { expected: number; received: number } | null {
    if (!Number.isSafeInteger(event.seq) || event.seq !== this.#next) {
      return { expected: this.#next, received: event.seq };
    }
    this.#next++;
    return null;
  }
}

/** One SSE frame -> one event, or null for a frame that carries none (a heartbeat, a comment).
 *
 * A comment is not an error and not an event: it is how a heartbeat is invisible to a consumer while
 * still keeping an idle proxy from closing the connection out from under a live status watch. */
function parseFrame(frame: string): StreamEvent | null {
  const data: string[] = [];
  for (const line of frame.split("\n")) {
    if (line === "" || line.startsWith(":")) continue; // comment / heartbeat
    const colon = line.indexOf(":");
    const field = colon === -1 ? line : line.slice(0, colon);
    // "data: x" and "data:x" are the same field; exactly one leading space is stripped.
    let value = colon === -1 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    // `event:`, `id:` and `retry:` are SSE fields this protocol does not use (v1 emits no id: lines
    // at all — §7). Ignoring them rather than failing is the same rule as ignoring an unknown event
    // type: a minor addition must not break an older client.
    if (field === "data") data.push(value);
  }
  if (data.length === 0) return null;

  try {
    return JSON.parse(data.join("\n")) as StreamEvent;
  } catch {
    // A frame we cannot parse is not a reason to tear down a live stream. Skip it: the protocol is
    // state-convergent, so the next snapshot cycle repairs whatever we missed.
    return null;
  }
}

/** What a stream event does to a store. This is the consumer's half of the event table (spec §4), and
 * it is exported because it IS the protocol — a host feeding a store from its own transport should
 * not have to reimplement the switch and get `synced` subtly wrong.
 *
 * Returns the paths that flashed, so a UI can highlight them. */
export function applyStreamEvent(store: LiveResourceStore, ev: StreamEvent): Path[] {
  switch (ev.type) {
    case "reset":
      store.beginSnapshot();
      return [];
    case "added":
    case "modified":
      if (!ev.object) return [];
      return store.applyServerEvent(ev.object, { redacted: ev.redacted }).flashed;
    case "deleted":
      if (ev.identity?.uid) store.removeResource(ev.identity.uid);
      return [];
    case "synced":
      store.endSnapshot();
      return [];
    default:
      // An unknown event type MUST be ignored, not treated as an error (spec §0). That is what lets
      // the gateway add an optional event type later without breaking a browser nobody can update.
      return [];
  }
}

export interface StreamOptions {
  /** Called for every `error` event. A terminal one has already closed the connection by the time
   * this returns — there is nothing to retry, and retrying is the bug. */
  onError?: (code: ErrorCode, message: string, terminal: boolean) => void;
  /** Called at the end of every snapshot cycle. The store is now consistent: a good moment to paint. */
  onSynced?: () => void;
  /** Called after any change, with the paths that flashed. */
  onChange?: (flashed: Path[]) => void;
  /** A missing or duplicated event was observed. The connection is closed; reconnect for a snapshot. */
  onGap?: (expected: number, received: number) => void;
  /** Abort the stream from outside. */
  signal?: AbortSignal;
  /** Injectable for tests. Defaults to the global fetch. */
  fetch?: typeof globalThis.fetch;
  headers?: Record<string, string>;
}

export interface StreamHandle {
  close(): void;
  /** Resolves when the stream ends: closed, aborted, or terminated by a terminal error. */
  closed: Promise<void>;
}

/** Consume a resource stream over fetch, feeding a store. Use this when the gateway authenticates
 * with a bearer token — native EventSource cannot send the header. */
export function connectResourceStream(url: string, store: LiveResourceStore, opts: StreamOptions = {}): StreamHandle {
  if (opts.signal?.aborted) return { close: () => {}, closed: Promise.resolve() };
  const controller = new AbortController();
  const fetchImpl = opts.fetch ?? globalThis.fetch;
  opts.signal?.addEventListener("abort", () => controller.abort(), { once: true });

  const closed = (async () => {
    const res = await fetchImpl(url, {
      signal: controller.signal,
      headers: { Accept: "text/event-stream", ...opts.headers },
      // The stream IS the response body; a cached one is a stream that never moves.
      cache: "no-store",
    });
    if (!res.ok || !res.body) {
      opts.onError?.("INTERNAL", `stream: HTTP ${res.status}`, true);
      return;
    }

    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    const decoder = new SSEDecoder();
    const sequence = new StreamSequence();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) return;
        for (const ev of decoder.push(value)) {
          if (feed(store, sequence, ev, opts)) {
            controller.abort(); // terminal: stop, and do NOT come back
            return;
          }
        }
      }
    } catch (err) {
      if (!controller.signal.aborted) throw err;
    } finally {
      reader.cancel().catch(() => {});
    }
  })();

  return {
    close: () => controller.abort(),
    closed: closed.catch(() => {}),
  };
}

/** Consume a resource stream with the browser's native EventSource. This is the same-origin
 * session-cookie path — the baseline a v1 gateway MUST support, because a cookie is the only
 * credential EventSource can carry. */
export function connectWithEventSource(
  url: string,
  store: LiveResourceStore,
  opts: Omit<StreamOptions, "fetch" | "headers"> = {},
): StreamHandle {
  if (opts.signal?.aborted) return { close: () => {}, closed: Promise.resolve() };
  const es = new EventSource(url, { withCredentials: true });
  const sequence = new StreamSequence();
  let resolve: () => void;
  const closed = new Promise<void>((r) => {
    resolve = r;
  });

  const shut = () => {
    es.close();
    resolve();
  };

  es.onmessage = (e: MessageEvent<string>) => {
    let ev: StreamEvent;
    try {
      ev = JSON.parse(e.data) as StreamEvent;
    } catch {
      return; // an unparseable frame is not a reason to tear down a live stream
    }
    if (feed(store, sequence, ev, opts)) shut();
  };

  // EventSource's `error` is also fired on a transport hiccup, where its OWN reconnect is the
  // correct behaviour and we must not interfere. Only a terminal PROTOCOL error (which arrives as a
  // message, above) closes the connection — and it must, or this reconnects forever.
  es.onerror = () => {
    if (es.readyState === EventSource.CLOSED) resolve();
  };

  opts.signal?.addEventListener("abort", shut, { once: true });
  return { close: shut, closed };
}

/** Apply one event; returns true if the stream must now be closed. */
function feed(store: LiveResourceStore, sequence: StreamSequence, ev: StreamEvent, opts: StreamOptions): boolean {
  const gap = sequence.observe(ev);
  if (gap) {
    opts.onGap?.(gap.expected, gap.received);
    return true;
  }
  if (ev.type === "error") {
    opts.onError?.(ev.code ?? "INTERNAL", ev.message ?? "", ev.terminal ?? false);
    return ev.terminal === true;
  }
  const flashed = applyStreamEvent(store, ev);
  if (ev.type === "synced") opts.onSynced?.();
  opts.onChange?.(flashed);
  return false;
}
