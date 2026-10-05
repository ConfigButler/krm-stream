// HTTP helpers both transports share: request options, the classification of a refused response,
// the Kubernetes Status message a host proxy refuses with, and `Retry-After`. Framing lives in the
// transports; nothing here reads an event.

import type { ErrorCode } from "./types.ts";

export interface RequestOptions {
  /** Defaults to same-origin; use include for a cross-origin cookie gateway or proxy. */
  credentials?: RequestCredentials;
  /** Injectable for tests. Defaults to the global fetch. */
  fetch?: typeof globalThis.fetch;
  headers?: Record<string, string>;
}

/** A GET that a long-lived stream can rely on: cancellable, uncached and with the caller's
 * credentials mode. A cached stream is a stream that never moves. */
export function request(url: string, accept: string, signal: AbortSignal, opts: RequestOptions): Promise<Response> {
  return (opts.fetch ?? globalThis.fetch)(url, {
    signal,
    headers: { Accept: accept, ...opts.headers },
    cache: "no-store",
    credentials: opts.credentials ?? "same-origin",
  });
}

/** How a refused HTTP status is reported: every 4xx except 408 and 429 is terminal, because asking
 * again cannot change the answer and a client that does will hammer a scope it may never see. */
export function refusal(status: number): { code: ErrorCode; terminal: boolean } {
  const code: ErrorCode =
    status === 401
      ? "UNAUTHENTICATED"
      : status === 403
        ? "FORBIDDEN"
        : status === 429 || status === 502 || status === 503 || status === 504
          ? "UPSTREAM_UNAVAILABLE"
          : "INTERNAL";
  return { code, terminal: status >= 400 && status < 500 && status !== 408 && status !== 429 };
}

/** The largest refusal body read in search of a Kubernetes Status message. */
const maxStatusBytes = 16 * 1024;
/** How long a refusal body may take to arrive. A known 401, 403 or 429 must not wait on a server
 * that stalls the body explaining it. */
const statusBudgetMs = 2_000;

/** The `message` of a Kubernetes `Status` body, as a host proxying `/k8s` refuses with, or
 * undefined. It reads at most maxStatusBytes for at most budgetMs, stops when `signal` aborts, and
 * always releases the body. */
export async function statusMessage(
  res: Response,
  signal: AbortSignal,
  budgetMs = statusBudgetMs,
): Promise<string | undefined> {
  if (!res.body) return undefined;
  if (signal.aborted || !/^application\/json\b/i.test(res.headers.get("Content-Type") ?? "")) {
    await res.body.cancel().catch(() => {});
    return undefined;
  }
  const reader = res.body.getReader();
  let gaveUp = false;
  // Cancelling the reader settles a pending read(), so a quiet body cannot hold this open.
  const giveUp = () => {
    gaveUp = true;
    void reader.cancel().catch(() => {});
  };
  const timer = setTimeout(giveUp, budgetMs);
  signal.addEventListener("abort", giveUp, { once: true });
  const chunks: Uint8Array[] = [];
  let size = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (gaveUp) return undefined;
      if (done) break;
      size += value.byteLength;
      if (size > maxStatusBytes) return undefined;
      chunks.push(value);
    }
    const bytes = new Uint8Array(size);
    let offset = 0;
    for (const chunk of chunks) {
      bytes.set(chunk, offset);
      offset += chunk.byteLength;
    }
    const status: unknown = JSON.parse(new TextDecoder().decode(bytes));
    if (typeof status !== "object" || status === null) return undefined;
    const { kind, message } = status as { kind?: unknown; message?: unknown };
    return kind === "Status" && typeof message === "string" && message !== "" ? message : undefined;
  } catch {
    return undefined;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener("abort", giveUp);
    await reader.cancel().catch(() => {});
  }
}

/** An HTTP `Retry-After` in milliseconds: delay-seconds or an HTTP-date. */
export function retryAfter(header: string | null, now = Date.now()): number | undefined {
  if (header === null) return undefined;
  const value = header.trim();
  if (/^\d+$/.test(value)) return Number(value) * 1000;
  const at = Date.parse(value);
  return Number.isNaN(at) ? undefined : Math.max(0, at - now);
}
