import { connectResourceStream, type StreamHandle, type StreamOptions } from "./sse.ts";
import type { LiveResourceStore } from "./store.ts";

export type ConnectionStatus = "connecting" | "syncing" | "live" | "retrying" | "closed" | "terminal" | "exhausted";

export interface ConnectionState {
  status: ConnectionStatus;
  /** Retries used during this connection's lifetime (never reset by a short-lived success). */
  retries: number;
  retryInMs?: number;
}

export interface ManagedStreamOptions extends StreamOptions {
  /** Total retry budget. Defaults to 8. Start a new connection to retry after exhaustion. */
  maxRetries?: number;
  /** Exponential backoff starts at 500ms, capped at 30s, with 50–100% jitter. */
  retryDelayMs?: number;
  maxRetryDelayMs?: number;
  onStateChange?: (state: Readonly<ConnectionState>) => void;
}

export interface ManagedStreamHandle extends StreamHandle {
  readonly state: Readonly<ConnectionState>;
  subscribe(callback: (state: Readonly<ConnectionState>) => void): () => void;
}

/** Managed fetch transport for both session cookies and bearer headers. Every reconnect requests a
 * fresh snapshot; the existing store and its drafts survive. HTTP 401/403 and terminal protocol
 * errors stop permanently. EOF, network failures and sequence gaps consume a bounded retry budget. */
export function connectManagedResourceStream(
  url: string,
  store: LiveResourceStore,
  opts: ManagedStreamOptions = {},
): ManagedStreamHandle {
  const maxRetries = opts.maxRetries ?? 8;
  const delay = opts.retryDelayMs ?? 500;
  const cap = opts.maxRetryDelayMs ?? 30_000;
  if (
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
  const publish = (status: ConnectionStatus, retryInMs?: number) => {
    state = Object.freeze({ status, retries: state.retries, ...(retryInMs === undefined ? {} : { retryInMs }) });
    opts.onStateChange?.(state);
    for (const callback of subscribers) callback(state);
  };
  const close = () => controller.abort();
  opts.signal?.addEventListener("abort", close, { once: true });
  if (opts.signal?.aborted) close();

  // Defer opening until the handle exists, so subscribers can observe the first transition.
  const closed = Promise.resolve().then(async () => {
    try {
      while (!controller.signal.aborted) {
        publish("connecting");
        if (controller.signal.aborted) break;
        const stream = connectResourceStream(url, store, {
          ...opts,
          signal: controller.signal,
          onOpen: () => {
            publish("syncing");
            opts.onOpen?.();
          },
          onChange: (change) => {
            if (change.type === "reset") publish("syncing");
            opts.onChange?.(change);
          },
          onSynced: () => {
            publish("live");
            opts.onSynced?.();
          },
          onError: (code, message, isTerminal) => {
            terminal ||= isTerminal;
            opts.onError?.(code, message, isTerminal);
          },
        });
        await stream.closed;
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
        const wait = Math.floor(ceiling * (0.5 + Math.random() * 0.5));
        state = { ...state, retries: state.retries + 1 };
        publish("retrying", wait);
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
