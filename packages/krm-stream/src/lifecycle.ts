// The connection lifecycle both connectors share: retries, the healthy-period reset, published state,
// cancellation and completion. It knows nothing about framing. A transport opens ONE connection,
// reports what happens on it through TransportHooks, and resolves when it ends; this file decides
// whether there is another.
//
// Internal on purpose. The two connectors in connection.ts and native.ts are the public surface; a
// general transport plugin API would be a contract nobody has asked for.

import type { ErrorCode, ResourceStateEvent } from "./types.ts";

export type ConnectionStatus = "connecting" | "syncing" | "live" | "retrying" | "closed" | "terminal" | "exhausted";

export interface ConnectionState {
  status: ConnectionStatus;
  /** Retries since the last sustained healthy period. */
  retries: number;
  retryInMs?: number;
  /** Present only on a `retrying` state that a sequence gap caused: the `seq` this connection expected
   * and the one it received. The event beyond the gap was discarded, and the retry asks for a fresh
   * snapshot. */
  gap?: Readonly<{ expected: number; received: number }>;
}

/** Receives every resource state event exactly once, synchronously and in stream order. It must
 * finish applying the event before it returns: `live` is published only after it has applied
 * `synced`. Do not pass an `async` function: it type-checks, because the result is ignored, but it
 * returns before applying anything and its exceptions never reach `closed`. An exception it throws
 * ends the stream; see ResourceStreamHandle.closed. */
export type ResourceEventConsumer = (event: ResourceStateEvent) => void;

export interface ResourceStreamOptions {
  /** Called for protocol and HTTP errors. A terminal error ends the stream without retry.
   * `retryAfterMs` is the server's hint for a retryable error: an error event's `retryAfterMs`, a
   * Kubernetes Status's `retryAfterSeconds`, or an HTTP `Retry-After`. */
  onError?: (code: ErrorCode, message: string, terminal: boolean, retryAfterMs?: number) => void;
  /** Abort the stream from outside. */
  signal?: AbortSignal;
  /** Defaults to same-origin; use include for a cross-origin cookie gateway or proxy. */
  credentials?: RequestCredentials;
  /** Injectable for tests. Defaults to the global fetch. */
  fetch?: typeof globalThis.fetch;
  headers?: Record<string, string>;
  /** Retry budget between sustained healthy periods. Defaults to 8. */
  maxRetries?: number;
  /** Continuous live time required to reset retries and backoff. Defaults to 30 seconds. */
  healthyResetMs?: number;
  /** Exponential backoff starts at 500ms, capped at 30s, with 50–100% jitter. */
  retryDelayMs?: number;
  maxRetryDelayMs?: number;
}

export interface ResourceStreamHandle {
  /** The current state. Read it for the initial presentation; `subscribe` for every later one. */
  readonly state: Readonly<ConnectionState>;
  subscribe(callback: (state: Readonly<ConnectionState>) => void): () => void;
  close(): void;
  /** Settles once the stream has ended — closed, aborted, terminal or exhausted — and its reader,
   * timers and listeners are released. It rejects, after the same clean-up, with the first exception
   * your own code threw: the consumer, a `subscribe` callback or `onError`, even after calling
   * close(). Attach a rejection handler, or a thrown render bug becomes an unhandled rejection. */
  closed: Promise<void>;
}

/** What one connection reports to the lifecycle. A host observes the same moments through the
 * handle's state, and the events themselves through its consumer. None of these throw: the lifecycle
 * catches the host's exceptions itself and aborts the signal instead, so a transport checks
 * `signal.aborted` after every call. */
export interface TransportHooks {
  /** Receives each state event synchronously, in stream order. */
  consume: (event: ResourceStateEvent) => void;
  /** The server accepted the request. Its snapshot has not started yet. */
  opened(): void;
  /** A snapshot is starting. Called before the consumer sees its `reset`. */
  reset(): void;
  /** The consumer has applied `synced` and the connection is still open: the stream is live. */
  synced(): void;
  /** A missing or duplicated event. The event beyond the gap was discarded; the connection ends. */
  gap(expected: number, received: number): void;
  /** A protocol or HTTP error. A terminal one ends the connection and the stream. */
  error(code: ErrorCode, message: string, terminal: boolean, retryAfterMs?: number): void;
}

/** Open one connection and report on it until it ends. It resolves in every case, after its readers
 * and listeners are released, because whether to come back is the lifecycle's decision. */
export type Transport = (hooks: TransportHooks, signal: AbortSignal) => Promise<void>;

/** Run `transport` until the stream ends, reconnecting within the bounded retry policy.
 *
 * An exception from the host's own code — the consumer, a `subscribe` callback or `onError` — is a
 * bug, not a network failure: retrying would turn it into a reconnect storm. It stops the stream at
 * once without a retry, every subscriber still sees the state being published, the final state is
 * `closed` (unless the stream had already ended `terminal` or `exhausted`), and `closed` rejects with
 * the first exception. */
export function runConnection(
  transport: Transport,
  consume: ResourceEventConsumer,
  opts: ResourceStreamOptions,
): ResourceStreamHandle {
  const maxRetries = opts.maxRetries ?? 8;
  const healthyResetMs = opts.healthyResetMs ?? 30_000;
  const delay = opts.retryDelayMs ?? 500;
  const cap = opts.maxRetryDelayMs ?? 30_000;
  if (
    !Number.isFinite(healthyResetMs) ||
    healthyResetMs <= 0 ||
    healthyResetMs > 2_147_483_647 ||
    !Number.isSafeInteger(maxRetries) ||
    maxRetries < 0 ||
    !Number.isFinite(delay) ||
    delay < 0 ||
    !Number.isFinite(cap) ||
    cap < 0 ||
    cap > 2_147_483_647
  ) {
    throw new RangeError("krm-stream: invalid retry budget or delay");
  }
  const controller = new AbortController();
  const subscribers = new Set<(state: Readonly<ConnectionState>) => void>();
  let state: Readonly<ConnectionState> = Object.freeze({ status: "connecting", retries: 0 });
  let terminal = false;
  let hintMs: number | undefined;
  let healthTimer: ReturnType<typeof setTimeout> | undefined;
  const clearHealthTimer = () => {
    clearTimeout(healthTimer);
    healthTimer = undefined;
  };
  // The first exception the host's own code threw. Recorded wherever it happens, so a close() from
  // inside the same callback cannot turn it into an ordinary ending.
  let failure: { error: unknown } | undefined;
  const fail = (error: unknown) => {
    failure ??= { error };
    clearHealthTimer();
    controller.abort();
  };
  const call = (callback: () => void) => {
    try {
      callback();
    } catch (error) {
      fail(error);
    }
  };
  const publish = (status: ConnectionStatus, detail: Pick<ConnectionState, "retryInMs" | "gap"> = {}) => {
    state = Object.freeze({ status, retries: state.retries, ...detail });
    for (const callback of subscribers) call(() => callback(state));
  };
  const close = () => {
    clearHealthTimer();
    controller.abort();
  };
  opts.signal?.addEventListener("abort", close, { once: true });
  if (opts.signal?.aborted) close();

  /** Connect and reconnect until the stream ends, and say how it ended. */
  const run = async (): Promise<"closed" | "terminal" | "exhausted"> => {
    while (!controller.signal.aborted) {
      publish("connecting");
      if (controller.signal.aborted) break;
      hintMs = undefined;
      let gap: ConnectionState["gap"];
      await transport(
        {
          consume: (event) => call(() => consume(event)),
          opened: () => publish("syncing"),
          reset: () => {
            clearHealthTimer();
            publish("syncing");
          },
          synced: () => {
            hintMs = undefined;
            if (state.status !== "live") {
              healthTimer = setTimeout(() => {
                healthTimer = undefined;
                if (!controller.signal.aborted && state.status === "live") {
                  state = { ...state, retries: 0 };
                  publish("live");
                }
              }, healthyResetMs);
            }
            publish("live");
          },
          gap: (expected, received) => {
            clearHealthTimer();
            gap = Object.freeze({ expected, received });
          },
          error: (code, message, isTerminal, retryAfterMs) => {
            if (isTerminal) clearHealthTimer();
            terminal ||= isTerminal;
            if (!isTerminal && retryAfterMs !== undefined) hintMs = retryAfterMs;
            call(() => opts.onError?.(code, message, isTerminal, retryAfterMs));
          },
        },
        controller.signal,
      );
      clearHealthTimer();
      if (controller.signal.aborted) break;
      if (terminal) return "terminal";
      if (state.retries >= maxRetries) return "exhausted";
      const ceiling = Math.min(cap, delay * 2 ** Math.min(state.retries, 30));
      const jittered = Math.floor(ceiling * (0.5 + Math.random() * 0.5));
      const wait = Math.min(cap, Math.max(jittered, hintMs ?? 0));
      state = { ...state, retries: state.retries + 1 };
      publish("retrying", { retryInMs: wait, ...(gap && { gap }) });
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          controller.signal.removeEventListener("abort", done);
          resolve();
        };
        const timer = setTimeout(done, wait);
        controller.signal.addEventListener("abort", done, { once: true });
        if (controller.signal.aborted) done();
      });
    }
    return "closed";
  };

  // Defer opening until the handle exists, so subscribers can observe the first transition.
  const closed = Promise.resolve().then(async () => {
    try {
      const ended = await run();
      // A host exception ends the stream as closed, whatever the loop was doing when it happened.
      publish(failure ? "closed" : ended);
    } finally {
      clearHealthTimer();
      opts.signal?.removeEventListener("abort", close);
      subscribers.clear();
    }
    if (failure) throw failure.error;
  });
  return {
    close,
    closed,
    get state() {
      return state;
    },
    subscribe(callback) {
      subscribers.add(callback);
      return () => {
        subscribers.delete(callback);
      };
    },
  };
}
