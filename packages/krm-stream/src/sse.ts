// The transport, consumer side. Everything else in this package is pure logic; this is the only file
// that knows a stream is made of bytes.
//
// One connection at a time, over fetch: fetch sends the same-origin session cookie a v1 gateway must
// accept (spec §7) and, unlike native EventSource, can also send `Authorization: Bearer`. It also
// works in Node, so it is what the conformance suite drives. lifecycle.ts owns everything that spans
// connections — retries, lifecycle state and the public handle — and connection.ts is the only caller.
//
// The rule that is easy to get wrong: on a TERMINAL error, close the connection and do not come back.
// A client that reconnects anyway will hammer a scope it can never be allowed to see — forever, from
// every open tab.
//
// Nothing here knows about a store. State events go to a consumer, and what it does with them is its
// own business.

import { type RequestOptions, refusal, request, retryAfter, statusMessage } from "./http.ts";
import type { TransportHooks } from "./lifecycle.ts";
import type { ResourceStateEvent, StreamEvent } from "./types.ts";
import { PROTOCOL_VERSION } from "./version.ts";

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

/** The state a decoded wire event carries, or null when it carries none: an error, a type this
 * client does not know (spec §0), or an upsert or deletion without the object or identity it needs.
 * This checks presence only; it is not schema validation of the object. */
export function toStateEvent(wire: StreamEvent): ResourceStateEvent | null {
  switch (wire.type) {
    case "reset":
      return {
        type: "reset",
        ...(wire.target === undefined ? {} : { target: wire.target }),
        ...(wire.scope === undefined ? {} : { scope: wire.scope }),
        ...(wire.projection === undefined ? {} : { projection: wire.projection }),
      };
    case "added":
    case "modified":
      if (!wire.object) return null;
      return {
        type: wire.type,
        object: wire.object,
        ...(wire.redacted === undefined ? {} : { redacted: wire.redacted }),
      };
    case "deleted":
      if (!wire.identity?.uid) return null;
      return { type: "deleted", identity: wire.identity };
    case "synced":
      return { type: "synced" };
    default:
      return null;
  }
}

/** Open one fetch connection and deliver its state events until it ends: EOF, a network failure, a
 * refusal, a terminal error, a sequence gap or `signal`.
 *
 * It resolves in every case, after the reader and listeners are released, because whether to come
 * back is the caller's decision. */
export async function streamOnce(
  url: string,
  hooks: TransportHooks,
  signal: AbortSignal,
  opts: RequestOptions = {},
): Promise<void> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  signal.addEventListener("abort", abort, { once: true });
  if (signal.aborted) abort();
  const sequence = new StreamSequence();
  // Called detached, so a consumer never sees this hooks object as `this`.
  const { consume } = hooks;

  /** Check and deliver one decoded event; false when the connection must end. */
  const deliver = (wire: StreamEvent): boolean => {
    // Sequence first, for every event, known or not: a gap makes everything after it uncertain.
    const gap = sequence.observe(wire);
    if (gap) {
      hooks.gap(gap.expected, gap.received);
      return false;
    }
    if (wire.type === "error") {
      const hint = typeof wire.retryAfterMs === "number" && wire.retryAfterMs >= 0 ? wire.retryAfterMs : undefined;
      hooks.error(wire.code ?? "INTERNAL", wire.message ?? "", wire.terminal ?? false, hint);
      return wire.terminal !== true;
    }
    const event = toStateEvent(wire);
    if (!event) return true;
    // Observed here, not inferred from what a consumer did with it: the connection is resyncing.
    if (event.type === "reset") {
      hooks.reset();
      if (controller.signal.aborted) return false;
    }
    consume(event);
    // A consumer that closed the stream — or threw — while applying `synced` gets no later `live`.
    if (event.type === "synced" && !controller.signal.aborted) hooks.synced();
    return true;
  };

  try {
    if (controller.signal.aborted) return;
    const res = await request(url, "text/event-stream", controller.signal, opts);
    if (!res.ok || !res.body) {
      const { code, terminal } = refusal(res.status);
      const message = (await statusMessage(res, controller.signal)) ?? `stream: HTTP ${res.status}`;
      if (controller.signal.aborted) return; // closed while reading the refusal: report nothing
      hooks.error(code, message, terminal, retryAfter(res.headers.get("Retry-After")));
      return;
    }
    if (controller.signal.aborted) {
      await res.body.cancel().catch(() => {});
      return;
    }
    // The header is optional (spec §0), and a cross-origin response may hide it. Only a version
    // that is present and different is refused, before a single event of it is applied.
    const protocol = res.headers.get("X-KRM-Stream-Protocol");
    if (protocol !== null && protocol.trim() !== String(PROTOCOL_VERSION)) {
      await res.body.cancel().catch(() => {});
      if (controller.signal.aborted) return;
      hooks.error(
        "INTERNAL",
        `stream: protocol mismatch: the gateway speaks X-KRM-Stream-Protocol ${JSON.stringify(protocol)}, this client speaks ${PROTOCOL_VERSION}`,
        true,
      );
      return;
    }

    hooks.opened();
    const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
    const cancelReader = () => {
      void reader.cancel().catch(() => {});
    };
    controller.signal.addEventListener("abort", cancelReader, { once: true });
    if (controller.signal.aborted) cancelReader();
    const decoder = new SSEDecoder();
    try {
      for (;;) {
        const { done, value } = await reader.read();
        if (done || controller.signal.aborted) return;
        for (const ev of decoder.push(value)) {
          // One chunk can hold many events. Nothing after a close is delivered.
          if (controller.signal.aborted) return;
          if (!deliver(ev)) {
            controller.abort(); // a gap or a terminal error: stop this connection
            return;
          }
        }
      }
    } finally {
      controller.signal.removeEventListener("abort", cancelReader);
      await reader.cancel().catch(() => {});
    }
  } catch {
    // The network failed. This connection is over, and the caller decides about the next. Host
    // callbacks never land here: the lifecycle catches their exceptions itself.
  } finally {
    signal.removeEventListener("abort", abort);
  }
}
