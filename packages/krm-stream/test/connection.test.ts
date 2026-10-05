import assert from "node:assert/strict";
import { test } from "node:test";
import {
  applyStreamEvent,
  type ConnectionState,
  type ConnectionStatus,
  connectResourceStream,
  LiveResourceStore,
  type ResourceStateEvent,
  type ResourceStreamHandle,
} from "../src/index.ts";

const encoder = new TextEncoder();
const sse = (events: unknown[]) => events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join("");
const response = (events: unknown[], init?: ResponseInit) => new Response(sse(events), init);
/** A body that arrives `size` characters at a time, so frames split wherever the slices fall. */
const chunked = (text: string, size: number) =>
  new Response(
    new ReadableStream({
      start(controller) {
        for (let i = 0; i < text.length; i += size) controller.enqueue(encoder.encode(text.slice(i, i + size)));
        controller.close();
      },
    }),
  );
/** A body that stays open after `text`, and records whether it was cancelled. */
function openBody(text: string, init?: ResponseInit) {
  const body = { cancelled: false };
  const res = new Response(
    new ReadableStream({
      start(controller) {
        controller.enqueue(encoder.encode(text));
      },
      cancel() {
        body.cancelled = true;
      },
    }),
    init,
  );
  return { response: res, body };
}
/** Every status the handle publishes from now on. Subscribe before the first microtask to see them all. */
function statuses(handle: ResourceStreamHandle): ConnectionStatus[] {
  const out: ConnectionStatus[] = [];
  handle.subscribe((state) => out.push(state.status));
  return out;
}
const ignore = () => {};

const object = (rv: string, value: string) => ({
  apiVersion: "v1",
  kind: "ConfigMap",
  metadata: { uid: "u", name: "cm", resourceVersion: rv },
  data: { value },
});
const identity = { uid: "u", apiVersion: "v1", kind: "ConfigMap", name: "cm" };

test("a gap discards the event beyond it, retries with gap context, and the next snapshot keeps the draft", async () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1", "base"));
  store.setValue("u", ["data", "value"], "draft");
  let calls = 0;
  const handle = connectResourceStream("/stream", (event) => applyStreamEvent(store, event), {
    retryDelayMs: 0,
    maxRetries: 2,
    fetch: async () =>
      ++calls === 1
        ? response([
            { seq: 1, type: "reset" },
            { seq: 3, type: "added", object: object("99", "must not apply") },
          ])
        : calls === 2
          ? response([
              { seq: 1, type: "reset" },
              { seq: 2, type: "added", object: object("2", "base") },
              { seq: 3, type: "synced" },
            ])
          : response([]),
  });
  const states: Readonly<ConnectionState>[] = [];
  handle.subscribe((state) => states.push(state));
  await handle.closed;
  assert.equal(calls, 3);
  assert.equal(store.server("u").metadata.resourceVersion, "2");
  assert.deepEqual(store.draft("u").data, { value: "draft" });

  const retries = states.filter((s) => s.status === "retrying");
  assert.equal(retries.length, 2);
  assert.deepEqual(retries[0]!.gap, { expected: 2, received: 3 }, "the gap is diagnosable from state");
  assert.equal(retries[1]!.gap, undefined, "an ordinary end carries no gap");
  const next = states[states.indexOf(retries[0]!) + 1]!;
  assert.deepEqual([next.status, next.gap], ["connecting", undefined], "the next attempt clears it");
  assert.ok(states.some((s) => s.status === "live"));
  assert.equal(handle.state.status, "exhausted");
});

test("a plain callback receives each state event once, in order, without seq or transport errors", async () => {
  const scope = { target: "demo", version: "v1", resource: "configmaps", namespace: "app" };
  const wire = sse([
    { seq: 1, type: "reset", target: "demo", scope, projection: "krm-full/v1", somethingNew: true },
    { seq: 2, type: "added", object: object("1", "a"), redacted: [] },
    // Unknown types still count in the sequence: seq 4 after this is not a gap.
    { seq: 3, type: "future-event", payload: 42 },
    // Nothing to apply without an object or a uid, so nothing is delivered.
    { seq: 4, type: "modified" },
    { seq: 5, type: "deleted", identity: { apiVersion: "v1", kind: "ConfigMap", name: "cm" } },
    { seq: 6, type: "synced" },
    { seq: 7, type: "error", code: "RESYNC_REQUIRED", message: "lost", terminal: false },
    { seq: 8, type: "deleted", identity },
  ]);
  const events: ResourceStateEvent[] = [];
  const errors: unknown[][] = [];
  const handle = connectResourceStream("/stream", (event) => events.push(event), {
    maxRetries: 0,
    fetch: async () => chunked(wire, 1),
    onError: (...args) => errors.push(args),
  });
  await handle.closed;
  assert.deepEqual(events, [
    { type: "reset", target: "demo", scope, projection: "krm-full/v1" },
    { type: "added", object: object("1", "a"), redacted: [] },
    { type: "synced" },
    { type: "deleted", identity },
  ]);
  assert.deepEqual(errors, [["RESYNC_REQUIRED", "lost", false, undefined]], "errors go only to onError");
  assert.equal(handle.state.status, "exhausted");
});

test("a wrapped store consumer can render each change, and a bound method keeps its receiver", async () => {
  const fetch = async () =>
    response([
      { seq: 1, type: "reset" },
      { seq: 2, type: "added", object: object("1", "a"), redacted: [] },
      { seq: 3, type: "synced" },
      { seq: 4, type: "modified", object: object("2", "b"), redacted: [] },
    ]);
  const store = new LiveResourceStore();
  const rendered: string[] = [];
  const wrapped = connectResourceStream(
    "/stream",
    (event) => {
      const change = applyStreamEvent(store, event);
      rendered.push(`${change.type} ${change.uid ?? "-"} ${change.flashed.map((p) => p.join(".")).join(",")}`);
    },
    { maxRetries: 0, fetch },
  );
  class Recorder {
    readonly types: string[] = [];
    consume = (event: ResourceStateEvent) => {
      this.types.push(event.type);
    };
  }
  const recorder = new Recorder();
  const direct = connectResourceStream("/stream", recorder.consume, { maxRetries: 0, fetch });
  await Promise.all([wrapped.closed, direct.closed]);
  assert.deepEqual(rendered, ["reset - ", "added u ", "synced - ", "modified u metadata.resourceVersion,data.value"]);
  assert.deepEqual(store.draft("u").data, { value: "b" });
  assert.deepEqual(recorder.types, ["reset", "added", "synced", "modified"]);
});

test("transport sees reset before the consumer applies it, and publishes live only after synced is applied", async () => {
  const log: string[] = [];
  const handle = connectResourceStream("/stream", (event) => log.push(`apply ${event.type}`), {
    maxRetries: 0,
    onError: (code) => log.push(`error ${code}`),
    fetch: async () =>
      response([
        { seq: 1, type: "reset" },
        { seq: 2, type: "added", object: object("1", "a") },
        { seq: 3, type: "synced" },
        { seq: 4, type: "error", code: "RESYNC_REQUIRED", terminal: false },
        { seq: 5, type: "reset" },
        { seq: 6, type: "synced" },
        // A completed snapshot publishes live even when the stream already was.
        { seq: 7, type: "synced" },
      ]),
  });
  handle.subscribe((state) => log.push(`state ${state.status}`));
  await handle.closed;
  assert.deepEqual(log, [
    "state connecting",
    "state syncing",
    "state syncing",
    "apply reset",
    "apply added",
    "apply synced",
    "state live",
    "error RESYNC_REQUIRED",
    "state syncing",
    "apply reset",
    "apply synced",
    "state live",
    "apply synced",
    "state live",
    "state exhausted",
  ]);
});

test("a consumer that closes while applying synced gets no live and no later event", async () => {
  const seen: string[] = [];
  const handle = connectResourceStream(
    "/stream",
    (event) => {
      seen.push(event.type);
      if (event.type === "synced") handle.close();
    },
    {
      fetch: async () =>
        response([
          { seq: 1, type: "reset" },
          { seq: 2, type: "synced" },
          { seq: 3, type: "added", object: object("1", "a") },
        ]),
    },
  );
  const states = statuses(handle);
  await handle.closed;
  assert.deepEqual(seen, ["reset", "synced"]);
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "closed"]);
});

/** Live timers in this process — node:test runs one file's tests one at a time. */
const pendingTimeouts = () => process.getActiveResourcesInfo().filter((r) => r === "Timeout").length;

/** An AbortSignal that knows which listeners are still attached to it. */
function trackedSignal() {
  const { signal } = new AbortController();
  const attached = new Set<unknown>();
  const add = signal.addEventListener.bind(signal);
  const remove = signal.removeEventListener.bind(signal);
  signal.addEventListener = (type: string, listener: EventListenerOrEventListenerObject, options?: unknown) => {
    attached.add(listener);
    add(type, listener, options as AddEventListenerOptions);
  };
  signal.removeEventListener = (type: string, listener: EventListenerOrEventListenerObject, options?: unknown) => {
    attached.delete(listener);
    remove(type, listener, options as EventListenerOptions);
  };
  return { signal, attached };
}

test("a consumer exception releases reader, timer and listener, never retries, and rejects closed with it", async () => {
  const boom = new Error("render failed");
  const { signal, attached } = trackedSignal();
  const errors: unknown[][] = [];
  const before = pendingTimeouts();
  let calls = 0;
  let body!: { cancelled: boolean };
  const seen: string[] = [];
  const handle = connectResourceStream(
    "/stream",
    (event) => {
      seen.push(event.type);
      if (event.type === "modified") throw boom;
    },
    {
      signal,
      retryDelayMs: 0,
      // Long enough that the health timer started by `live` would still be pending if it leaked.
      healthyResetMs: 60_000,
      onError: (...args) => errors.push(args),
      fetch: async () => {
        calls++;
        const opened = openBody(
          sse([
            { seq: 1, type: "reset" },
            { seq: 2, type: "added", object: object("1", "a") },
            { seq: 3, type: "synced" },
            { seq: 4, type: "modified", object: object("2", "b") },
            { seq: 5, type: "modified", object: object("3", "c") },
          ]),
        );
        body = opened.body;
        return opened.response;
      },
    },
  );
  const states = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(calls, 1, "a consumer exception is not retried");
  assert.deepEqual(seen, ["reset", "added", "synced", "modified"], "nothing after the exception is delivered");
  assert.deepEqual(errors, [], "it is not reported as a stream error");
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "live", "closed"]);
  assert.equal(handle.state.status, "closed");
  assert.equal(body.cancelled, true, "the reader was cancelled");
  assert.equal(pendingTimeouts(), before, "the health timer was cleared");
  assert.equal(attached.size, 0, "no listener is left on the caller's signal");
});

test("close() and then throw still rejects closed with the original exception", async () => {
  const boom = new Error("thrown after close");
  let calls = 0;
  const handle = connectResourceStream(
    "/stream",
    () => {
      handle.close();
      throw boom;
    },
    {
      retryDelayMs: 0,
      fetch: async () => {
        calls++;
        return response([
          { seq: 1, type: "reset" },
          { seq: 2, type: "synced" },
        ]);
      },
    },
  );
  const states = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(calls, 1);
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "closed"]);
});

test("even a thrown undefined is kept, not mistaken for a clean end", async () => {
  const handle = connectResourceStream(
    "/stream",
    () => {
      throw undefined;
    },
    { fetch: async () => response([{ seq: 1, type: "reset" }]) },
  );
  const outcome = await handle.closed.then(
    () => "resolved",
    (error: unknown) => ({ error }),
  );
  assert.deepEqual(outcome, { error: undefined });
  assert.equal(handle.state.status, "closed");
});

// A subscriber or onError that throws is the host's bug, exactly like a consumer that throws. Before,
// one thrown during a connection read as a network failure and retried until the budget ran out — a
// broken render function became a reconnect storm ending in `exhausted`.
test("a subscriber that throws during a connection stops the stream without retrying", async () => {
  const boom = new Error("render failed");
  let calls = 0;
  let body!: { cancelled: boolean };
  const handle = connectResourceStream("/stream", ignore, {
    retryDelayMs: 0,
    fetch: async () => {
      calls++;
      const opened = openBody(
        sse([
          { seq: 1, type: "reset" },
          { seq: 2, type: "synced" },
        ]),
      );
      body = opened.body;
      return opened.response;
    },
  });
  handle.subscribe((state) => {
    if (state.status === "live") throw boom;
  });
  const others = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(calls, 1, "a host bug is not retried");
  assert.equal(body.cancelled, true);
  assert.deepEqual(others, ["connecting", "syncing", "syncing", "live", "closed"], "every subscriber still hears it");
  assert.equal(handle.state.status, "closed");
});

test("a subscriber that throws between connections never opens another", async () => {
  for (const at of ["connecting", "retrying"] as const) {
    const boom = new Error(`threw on ${at}`);
    let calls = 0;
    const handle = connectResourceStream("/stream", ignore, {
      retryDelayMs: 0,
      fetch: async () => {
        calls++;
        throw new Error("offline");
      },
    });
    handle.subscribe((state) => {
      if (state.status === at) throw boom;
    });
    const states = statuses(handle);
    await assert.rejects(handle.closed, (error) => error === boom);
    assert.equal(calls, at === "connecting" ? 0 : 1, at);
    assert.equal(states.at(-1), "closed", at);
  }
});

test("a live republished by the health timer that throws ends the stream", async () => {
  const boom = new Error("threw on the health reset");
  let body!: { cancelled: boolean };
  const handle = connectResourceStream("/stream", ignore, {
    healthyResetMs: 5,
    fetch: async () => {
      const opened = openBody(
        sse([
          { seq: 1, type: "reset" },
          { seq: 2, type: "synced" },
        ]),
      );
      body = opened.body;
      return opened.response;
    },
  });
  let lives = 0;
  handle.subscribe((state) => {
    if (state.status === "live" && ++lives === 2) throw boom;
  });
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(body.cancelled, true);
  assert.equal(handle.state.status, "closed");
});

test("onError that throws stops the stream instead of retrying", async () => {
  const boom = new Error("could not show the error");
  let calls = 0;
  const handle = connectResourceStream("/stream", ignore, {
    retryDelayMs: 0,
    onError: () => {
      throw boom;
    },
    fetch: async () => {
      calls++;
      return new Response(null, { status: 503 });
    },
  });
  const states = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(calls, 1);
  assert.deepEqual(states, ["connecting", "closed"]);
});

test("a matching or absent protocol header is accepted", async () => {
  for (const headers of [{ "X-KRM-Stream-Protocol": "1" }, {}] as Record<string, string>[]) {
    const seen: string[] = [];
    const handle = connectResourceStream("/stream", (event) => seen.push(event.type), {
      maxRetries: 0,
      fetch: async () =>
        response(
          [
            { seq: 1, type: "reset" },
            { seq: 2, type: "synced" },
          ],
          { headers },
        ),
    });
    await handle.closed;
    assert.deepEqual(seen, ["reset", "synced"], JSON.stringify(headers));
    assert.equal(handle.state.status, "exhausted");
  }
});

test("a different protocol version applies nothing, is terminal, and is never retried", async () => {
  let calls = 0;
  let body!: { cancelled: boolean };
  const seen: string[] = [];
  const errors: unknown[][] = [];
  const handle = connectResourceStream(
    "/stream",
    (event) => {
      seen.push(event.type);
      handle.close(); // fail fast rather than hang on the open body if an event ever gets through
    },
    {
      retryDelayMs: 0,
      onError: (...args) => errors.push(args),
      fetch: async () => {
        calls++;
        const opened = openBody(sse([{ seq: 1, type: "reset" }]), { headers: { "X-KRM-Stream-Protocol": "2" } });
        body = opened.body;
        return opened.response;
      },
    },
  );
  const states = statuses(handle);
  await handle.closed;
  assert.equal(calls, 1);
  assert.deepEqual(seen, []);
  assert.equal(body.cancelled, true);
  assert.deepEqual(states, ["connecting", "terminal"]);
  assert.equal(errors.length, 1);
  const [code, message, terminal] = errors[0]!;
  assert.equal(code, "INTERNAL");
  assert.equal(terminal, true);
  assert.match(String(message), /protocol mismatch.*"2".*1/);
});

test("an HTTP refusal keeps its own classification whatever protocol it names", async () => {
  const errors: unknown[][] = [];
  const handle = connectResourceStream("/stream", ignore, {
    onError: (...args) => errors.push(args),
    fetch: async () => new Response(null, { status: 403, headers: { "X-KRM-Stream-Protocol": "2" } }),
  });
  await handle.closed;
  assert.deepEqual(errors, [["FORBIDDEN", "stream: HTTP 403", true, undefined]]);
  assert.equal(handle.state.status, "terminal");
});

for (const status of [401, 403])
  test(`HTTP ${status} is terminal`, async () => {
    let calls = 0;
    const handle = connectResourceStream("/stream", ignore, {
      fetch: async () => {
        calls++;
        return new Response(null, { status });
      },
      retryDelayMs: 0,
    });
    await handle.closed;
    assert.equal(calls, 1);
    assert.equal(handle.state.status, "terminal");
  });

test("terminal protocol errors never reconnect", async () => {
  const handle = connectResourceStream("/stream", ignore, {
    fetch: async () => response([{ seq: 1, type: "error", code: "FORBIDDEN", terminal: true }]),
  });
  await handle.closed;
  assert.equal(handle.state.status, "terminal");
  assert.equal(handle.state.retries, 0);
});

for (const failure of ["network", "http", "eof"])
  test(`${failure} consumes exactly the retry budget`, async () => {
    let calls = 0;
    const handle = connectResourceStream("/stream", ignore, {
      maxRetries: 2,
      retryDelayMs: 0,
      fetch: async () => {
        calls++;
        if (failure === "network") throw new Error("offline");
        return new Response(null, { status: failure === "http" ? 503 : 200 });
      },
    });
    await handle.closed;
    assert.equal(calls, 3);
    assert.equal(handle.state.status, "exhausted");
  });

test("abort during backoff cancels the timer and never opens another connection", async () => {
  const controller = new AbortController();
  let calls = 0;
  const handle = connectResourceStream("/stream", ignore, {
    signal: controller.signal,
    fetch: async () => {
      calls++;
      throw new Error("offline");
    },
  });
  handle.subscribe((state) => {
    if (state.status === "retrying") controller.abort();
  });
  await handle.closed;
  assert.equal(calls, 1);
  assert.equal(handle.state.status, "closed");
});

test("an already aborted stream never fetches", async () => {
  const handle = connectResourceStream("/stream", ignore, {
    signal: AbortSignal.abort(),
    fetch: async () => {
      assert.fail("opened");
    },
  });
  await handle.closed;
  assert.equal(handle.state.status, "closed");
});

test("closing an active stream cancels the reader and delivers nothing more from the same chunk", async () => {
  const { response: res, body } = openBody(
    sse([
      { seq: 1, type: "reset" },
      { seq: 2, type: "synced" },
    ]),
  );
  let consumed = 0;
  const handle = connectResourceStream(
    "/stream",
    () => {
      consumed++;
      handle.close();
    },
    { fetch: async () => res },
  );
  await handle.closed;
  assert.equal(consumed, 1);
  assert.equal(body.cancelled, true);
  assert.equal(handle.state.status, "closed");
});

test("abort cancels a quiet response reader", async () => {
  const { response: res, body } = openBody("");
  const handle = connectResourceStream("/stream", ignore, { fetch: async () => res });
  await new Promise<void>((resolve) => {
    handle.subscribe((state) => {
      if (state.status === "syncing") resolve();
    });
  });
  handle.close();
  await handle.closed;
  assert.equal(body.cancelled, true);
  assert.equal(handle.state.status, "closed");
});

test("sustained live periods replenish retries and backoff across an all-day connection", async () => {
  let attempts = 0;
  let body: ReadableStreamDefaultController<Uint8Array>;
  const waits: number[] = [];
  const handle = connectResourceStream("/stream", ignore, {
    maxRetries: 1,
    healthyResetMs: 10,
    retryDelayMs: 2,
    fetch: async () => {
      attempts++;
      return new Response(
        new ReadableStream({
          start(controller) {
            body = controller;
            controller.enqueue(encoder.encode('data: {"seq":1,"type":"reset"}\n\ndata: {"seq":2,"type":"synced"}\n\n'));
          },
        }),
      );
    },
  });
  handle.subscribe((state) => {
    if (state.status === "retrying") waits.push(state.retryInMs!);
    // Health reset republishes live with zero retries. End each healthy attempt there.
    if (state.status === "live" && state.retries === 0 && attempts > 1) {
      if (attempts === 4) handle.close();
      else body.close();
    }
  });
  // First attempt starts at zero retries; wait until its initial health period has elapsed.
  await new Promise<void>((resolve) => {
    let lives = 0;
    const stop = handle.subscribe((state) => {
      if (state.status === "live" && ++lives === 2) {
        stop();
        body.close();
        resolve();
      }
    });
  });
  await handle.closed;
  assert.equal(attempts, 4);
  assert.equal(waits.length, 3);
  assert.ok(waits.every((delay) => delay <= 2));
  assert.equal(handle.state.status, "closed");
});

test("brief synced connections still exhaust their retry budget", async () => {
  let attempts = 0;
  const handle = connectResourceStream("/stream", ignore, {
    maxRetries: 2,
    retryDelayMs: 0,
    healthyResetMs: 1000,
    fetch: async () => {
      attempts++;
      return response([
        { seq: 1, type: "reset" },
        { seq: 2, type: "synced" },
      ]);
    },
  });
  await handle.closed;
  assert.equal(attempts, 3);
  assert.equal(handle.state.status, "exhausted");
});

test("snapshot resets restart the health interval and close removes the timer", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  let attempts = 0;
  let body!: ReadableStreamDefaultController<Uint8Array>;
  const frame = (seq: number, type: string) => encoder.encode(`data: ${JSON.stringify({ seq, type })}\n\n`);
  const handle = connectResourceStream("/stream", ignore, {
    retryDelayMs: 0,
    healthyResetMs: 30,
    fetch: async () => {
      if (++attempts === 1) throw new Error("offline");
      return new Response(
        new ReadableStream({
          start(controller) {
            body = controller;
            controller.enqueue(frame(1, "reset"));
            controller.enqueue(frame(2, "synced"));
          },
        }),
      );
    },
  });
  const flush = () => new Promise<void>((resolve) => setImmediate(resolve));
  await flush();
  t.mock.timers.tick(0);
  await flush();
  assert.equal(handle.state.retries, 1);
  assert.equal(handle.state.status, "live");
  t.mock.timers.tick(20);
  body.enqueue(frame(3, "reset"));
  body.enqueue(frame(4, "synced"));
  await flush();
  t.mock.timers.tick(20);
  assert.equal(handle.state.retries, 1);
  t.mock.timers.tick(10);
  assert.equal(handle.state.retries, 0);
  handle.close();
  await handle.closed;
  t.mock.timers.tick(100);
  assert.equal(handle.state.status, "closed");
});

/** Runs a stream until its first retry is scheduled, and returns how long it would wait. */
async function firstRetryDelay(fetch: typeof globalThis.fetch, opts: { maxRetryDelayMs?: number } = {}) {
  let delay: number | undefined;
  const handle = connectResourceStream("/stream", ignore, { retryDelayMs: 0, ...opts, fetch });
  handle.subscribe((s) => {
    if (s.status === "retrying" && delay === undefined) {
      delay = s.retryInMs;
      handle.close();
    }
  });
  await handle.closed;
  return delay;
}

test("a retryable error's retryAfterMs sets the least the reconnect waits", async () => {
  const errors: unknown[][] = [];
  const delay = await firstRetryDelay(async () =>
    response([{ seq: 1, type: "error", code: "UPSTREAM_UNAVAILABLE", terminal: false, retryAfterMs: 1500 }]),
  );
  assert.equal(delay, 1500);

  const handle = connectResourceStream("/stream", ignore, {
    maxRetries: 0,
    fetch: async () =>
      response([{ seq: 1, type: "error", code: "UPSTREAM_UNAVAILABLE", terminal: false, retryAfterMs: 1500 }]),
    onError: (...args) => errors.push(args),
  });
  await handle.closed;
  assert.deepEqual(errors, [["UPSTREAM_UNAVAILABLE", "", false, 1500]]);
});

test("HTTP Retry-After is honoured within the client's own cap", async () => {
  const fetch = async () => new Response(null, { status: 429, headers: { "Retry-After": "2" } });
  assert.equal(await firstRetryDelay(fetch), 2000);
  assert.equal(await firstRetryDelay(fetch, { maxRetryDelayMs: 300 }), 300);
});

test("a hint is discarded once a snapshot completes on the same connection", async () => {
  const delay = await firstRetryDelay(async () =>
    response([
      { seq: 1, type: "error", code: "RESYNC_REQUIRED", terminal: false, retryAfterMs: 5000 },
      { seq: 2, type: "reset" },
      { seq: 3, type: "synced" },
    ]),
  );
  assert.equal(delay, 0);
});

test("a refusal's Kubernetes Status message is shown instead of the bare status", async () => {
  const messages: string[] = [];
  const refuse =
    (body: string, contentType = "application/json") =>
    async () =>
      new Response(body, { status: 403, headers: { "Content-Type": contentType } });
  for (const fetch of [
    refuse(JSON.stringify({ kind: "Status", code: 403, message: 'notes is forbidden: User "carol" cannot watch' })),
    refuse(JSON.stringify({ kind: "Status", message: "not json" }), "text/plain"),
    refuse(JSON.stringify({ kind: "Status", message: "x".repeat(20_000) })),
    refuse("{not json"),
  ]) {
    const handle = connectResourceStream("/stream", ignore, {
      fetch,
      onError: (_code, message) => messages.push(message),
    });
    await handle.closed;
  }
  assert.deepEqual(messages, [
    'notes is forbidden: User "carol" cannot watch',
    "stream: HTTP 403",
    "stream: HTTP 403",
    "stream: HTTP 403",
  ]);
});

/** A JSON refusal whose body sends `prefix` and then goes quiet. Records whether it was cancelled. */
function stalledRefusal(prefix = "") {
  const body = { cancelled: false };
  const response = new Response(
    new ReadableStream<Uint8Array>({
      start(controller) {
        if (prefix) controller.enqueue(encoder.encode(prefix));
      },
      cancel() {
        body.cancelled = true;
      },
    }),
    { status: 403, headers: { "Content-Type": "application/json" } },
  );
  return { response, body };
}

test("close() while a refusal body is quiet ends the stream and cancels the body", async () => {
  const { response, body } = stalledRefusal();
  const errors: string[] = [];
  let fetched!: () => void;
  const wasFetched = new Promise<void>((resolve) => {
    fetched = resolve;
  });
  const handle = connectResourceStream("/stream", ignore, {
    fetch: async () => {
      fetched();
      return response;
    },
    onError: (_code, message) => errors.push(message),
  });
  await wasFetched;
  await new Promise((resolve) => setTimeout(resolve, 10));
  handle.close();
  await handle.closed;
  assert.equal(handle.state.status, "closed");
  assert.equal(body.cancelled, true);
  assert.deepEqual(errors, [], "a closed stream reports nothing");
});

for (const [name, prefix] of [
  ["quiet", ""],
  ["partial", '{"kind":"Status","mess'],
] as const)
  test(`a ${name} refusal body falls back to the bare status within its budget`, async () => {
    const { statusMessage } = await import("../src/http.ts");
    const { response, body } = stalledRefusal(prefix);
    const started = Date.now();
    assert.equal(await statusMessage(response, new AbortController().signal, 20), undefined);
    assert.ok(Date.now() - started < 1_000, "the budget bounds the read");
    assert.equal(body.cancelled, true);
  });

test("aborting during a refusal-body read stops it at once", async () => {
  const { statusMessage } = await import("../src/http.ts");
  const { response, body } = stalledRefusal('{"kind":');
  const abort = new AbortController();
  const read = statusMessage(response, abort.signal, 60_000);
  setTimeout(() => abort.abort(), 10);
  assert.equal(await read, undefined);
  assert.equal(body.cancelled, true);
});
