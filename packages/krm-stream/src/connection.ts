import { streamOnce } from "./sse.ts";
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
 * `synced`. An exception it throws ends the stream; see ResourceStreamHandle.closed. */
export type ResourceEventConsumer = (event: ResourceStateEvent) => void;

export interface ResourceStreamOptions {
  /** Called for protocol and HTTP errors. A terminal error ends the stream without retry.
   * `retryAfterMs` is the server's hint for a retryable error: an error event's `retryAfterMs`, or
   * an HTTP `Retry-After`. */
  onError?: (code: ErrorCode, message: string, terminal: boolean, retryAfterMs?: number) => void;
  /** Abort the stream from outside. */
  signal?: AbortSignal;
  /** Defaults to same-origin; use include for a cross-origin cookie gateway. */
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
   * timers and listeners are released. It rejects, after the same clean-up and a `closed` state, with
   * the consumer's own exception when the consumer threw, even if it called close() first. */
  closed: Promise<void>;
}

/** Consume a resource stream over fetch, for both session cookies and bearer headers, delivering
 * each state event to `consume`. Every reconnect requests a fresh snapshot; whatever the consumer
 * holds survives it. HTTP 401/403, terminal protocol errors and a different protocol version stop
 * permanently. EOF, network failures, sequence gaps and retryable errors such as
 * UPSTREAM_UNAVAILABLE consume a bounded retry budget. A consumer exception stops at once.
 *
 * A server's retry hint (an HTTP `Retry-After`, or an error event's `retryAfterMs`) sets the least
 * the next reconnect waits, within `maxRetryDelayMs`. A non-terminal event on an open connection does
 * not itself reconnect: the gateway recovers RESYNC_REQUIRED in band, and closes the connection
 * after any error it wants the client to retry. A completed snapshot discards the hint. */
export function connectResourceStream(
  url: string,
  consume: ResourceEventConsumer,
  opts: ResourceStreamOptions = {},
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
  const publish = (status: ConnectionStatus, detail: Pick<ConnectionState, "retryInMs" | "gap"> = {}) => {
    state = Object.freeze({ status, retries: state.retries, ...detail });
    for (const callback of subscribers) callback(state);
  };
  const close = () => {
    clearHealthTimer();
    controller.abort();
  };
  opts.signal?.addEventListener("abort", close, { once: true });
  if (opts.signal?.aborted) close();

  // Defer opening until the handle exists, so subscribers can observe the first transition.
  const closed = Promise.resolve().then(async () => {
    try {
      while (!controller.signal.aborted) {
        publish("connecting");
        if (controller.signal.aborted) break;
        hintMs = undefined;
        let gap: ConnectionState["gap"];
        const failure = await streamOnce(
          url,
          {
            consume,
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
              opts.onError?.(code, message, isTerminal, retryAfterMs);
            },
          },
          controller.signal,
          opts,
        );
        clearHealthTimer();
        if (failure) {
          // Not a Kubernetes error and not retryable: the application's own code failed. The reader is
          // already released; report the end, then the exception itself.
          publish("closed");
          throw failure.error;
        }
        if (controller.signal.aborted) break;
        if (terminal) {
          publish("terminal");
          return;
        }
        if (state.retries >= maxRetries) {
          publish("exhausted");
          return;
        }
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
      publish("closed");
    } finally {
      clearHealthTimer();
      opts.signal?.removeEventListener("abort", close);
      subscribers.clear();
    }
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
