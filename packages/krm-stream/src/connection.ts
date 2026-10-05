import {
  type ResourceEventConsumer,
  type ResourceStreamHandle,
  type ResourceStreamOptions,
  runConnection,
} from "./lifecycle.ts";
import { streamOnce } from "./sse.ts";

export type {
  ConnectionState,
  ConnectionStatus,
  ResourceEventConsumer,
  ResourceStreamHandle,
  ResourceStreamOptions,
} from "./lifecycle.ts";

/** Consume a gateway resource stream over fetch, for both session cookies and bearer headers,
 * delivering each state event to `consume`. Every reconnect requests a fresh snapshot; whatever the
 * consumer holds survives it. Every HTTP 4xx except 408/429, terminal protocol errors and a
 * different protocol version stop permanently. EOF, network failures, sequence gaps and retryable
 * errors such as UPSTREAM_UNAVAILABLE consume a bounded retry budget.
 *
 * An exception from the host's own code — the consumer, a `subscribe` callback or `onError` — is a
 * bug, not a network failure: retrying would turn it into a reconnect storm. It stops the stream at
 * once without a retry, every subscriber still sees the state being published, the final state is
 * `closed` (unless the stream had already ended `terminal` or `exhausted`), and `closed` rejects with
 * the first exception.
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
  return runConnection((hooks, signal) => streamOnce(url, hooks, signal, opts), consume, opts);
}
