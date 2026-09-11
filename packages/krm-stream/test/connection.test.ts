import assert from "node:assert/strict";
import { test } from "node:test";
import { type ConnectionStatus, connectManagedResourceStream, LiveResourceStore } from "../src/index.ts";

const response = (events: unknown[]) => new Response(events.map((e) => `data: ${JSON.stringify(e)}\n\n`).join(""));
const object = (rv: string, value: string) => ({
  apiVersion: "v1",
  kind: "ConfigMap",
  metadata: { uid: "u", name: "cm", resourceVersion: rv },
  data: { value },
});

test("managed recovery rejects a gap, resnapshots, and preserves a draft", async () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1", "base"));
  store.setValue("u", ["data", "value"], "draft");
  let calls = 0;
  const states: ConnectionStatus[] = [];
  const handle = connectManagedResourceStream("/stream", store, {
    retryDelayMs: 0,
    maxRetries: 1,
    onStateChange: (s) => states.push(s.status),
    fetch: async () =>
      ++calls === 1
        ? response([
            { seq: 1, type: "reset" },
            { seq: 3, type: "added", object: object("99", "must not apply") },
          ])
        : response([
            { seq: 1, type: "reset" },
            { seq: 2, type: "added", object: object("2", "base") },
            { seq: 3, type: "synced" },
          ]),
  });
  await handle.closed;
  assert.equal(calls, 2);
  assert.equal(store.server("u").metadata.resourceVersion, "2");
  assert.deepEqual(store.draft("u").data, { value: "draft" });
  assert.ok(states.includes("retrying"));
  assert.ok(states.includes("live"));
  assert.equal(handle.state.status, "exhausted");
});

for (const status of [401, 403])
  test(`HTTP ${status} is terminal`, async () => {
    let calls = 0;
    const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
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
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
    fetch: async () => response([{ seq: 1, type: "error", code: "FORBIDDEN", terminal: true }]),
  });
  await handle.closed;
  assert.equal(handle.state.status, "terminal");
  assert.equal(handle.state.retries, 0);
});

for (const failure of ["network", "http", "eof"])
  test(`${failure} consumes exactly the retry budget`, async () => {
    let calls = 0;
    const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
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
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
    signal: controller.signal,
    fetch: async () => {
      calls++;
      throw new Error("offline");
    },
    onStateChange: (state) => {
      if (state.status === "retrying") controller.abort();
    },
  });
  await handle.closed;
  assert.equal(calls, 1);
  assert.equal(handle.state.status, "closed");
});

test("already aborted managed stream never fetches", async () => {
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
    signal: AbortSignal.abort(),
    fetch: async () => {
      assert.fail("opened");
    },
  });
  await handle.closed;
  assert.equal(handle.state.status, "closed");
});

test("closing an active stream cancels the reader and prevents buffered events after close", async () => {
  const store = new LiveResourceStore();
  let canceled = false;
  let changes = 0;
  const handle = connectManagedResourceStream("/stream", store, {
    fetch: async () =>
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(
              new TextEncoder().encode('data: {"seq":1,"type":"reset"}\n\ndata: {"seq":2,"type":"synced"}\n\n'),
            );
          },
          cancel() {
            canceled = true;
          },
        }),
      ),
    onChange: () => {
      changes++;
      handle.close();
    },
  });
  await handle.closed;
  assert.equal(changes, 1);
  assert.equal(canceled, true);
  assert.equal(handle.state.status, "closed");
});

test("abort cancels a quiet response reader", async () => {
  let markOpen!: () => void;
  const opened = new Promise<void>((resolve) => {
    markOpen = resolve;
  });
  let canceled = false;
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
    onOpen: markOpen,
    fetch: async () =>
      new Response(
        new ReadableStream({
          cancel() {
            canceled = true;
          },
        }),
      ),
  });
  await opened;
  handle.close();
  await handle.closed;
  assert.equal(canceled, true);
  assert.equal(handle.state.status, "closed");
});

test("sustained live periods replenish retries and backoff across an all-day connection", async () => {
  let attempts = 0;
  let body: ReadableStreamDefaultController<Uint8Array>;
  const waits: number[] = [];
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
    maxRetries: 1,
    healthyResetMs: 10,
    retryDelayMs: 2,
    fetch: async () => {
      attempts++;
      return new Response(
        new ReadableStream({
          start(controller) {
            body = controller;
            controller.enqueue(
              new TextEncoder().encode('data: {"seq":1,"type":"reset"}\n\ndata: {"seq":2,"type":"synced"}\n\n'),
            );
          },
        }),
      );
    },
    onStateChange: (state) => {
      if (state.status === "retrying") waits.push(state.retryInMs!);
      // Health reset republishes live with zero retries. End each healthy attempt there.
      if (state.status === "live" && state.retries === 0 && attempts > 1) {
        if (attempts === 4) handle.close();
        else body.close();
      }
    },
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
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
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
  const frame = (seq: number, type: string) => new TextEncoder().encode(`data: ${JSON.stringify({ seq, type })}\n\n`);
  const handle = connectManagedResourceStream("/stream", new LiveResourceStore(), {
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
