// The native connector against a scripted fake fetch: every LIST and WATCH request is answered by
// the next step of a script, so each test decides exactly what the "API server" says and in which
// chunks. The lifecycle itself is shared with the gateway connector and covered in
// connection.test.ts; these tests cover what native transport adds and how it maps onto that
// lifecycle.

import assert from "node:assert/strict";
import { test } from "node:test";
import {
  applyStreamEvent,
  type ConnectionState,
  type ConnectionStatus,
  connectNativeWatch,
  LiveResourceStore,
  nativeCollectionURL,
  nativeObjectURL,
  type ResourceStateEvent,
  type ResourceStreamHandle,
  readOnlyPolicy,
} from "../src/index.ts";

const encoder = new TextEncoder();
const ignore = () => {};

const cm = (uid: string, name: string, rv: string, value = "v") => ({
  apiVersion: "v1",
  kind: "ConfigMap",
  metadata: { uid, name, namespace: "app", resourceVersion: rv },
  data: { value },
});
const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json", ...headers } });
const collection = (
  items: unknown[],
  rv = "10",
  extra: { kind?: string; apiVersion?: string; continue?: string } = {},
) =>
  json({
    kind: extra.kind ?? "ConfigMapList",
    apiVersion: extra.apiVersion ?? "v1",
    metadata: { resourceVersion: rv, ...(extra.continue === undefined ? {} : { continue: extra.continue }) },
    items,
  });
const status = (code: number, message: string, extra: Record<string, unknown> = {}) => ({
  kind: "Status",
  apiVersion: "v1",
  status: "Failure",
  message,
  code,
  ...extra,
});
const frames = (...events: unknown[]) => events.map((e) => `${JSON.stringify(e)}\n`).join("");

/** A watch body. `chunk` splits it every that many BYTES, so frames and characters split anywhere.
 * `open` keeps it open after the text; `push` and `end` drive it later. */
function body(text: string, opts: { chunk?: number; open?: boolean } = {}) {
  const state = { cancelled: false };
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const bytes = encoder.encode(text);
  const size = opts.chunk ?? Math.max(bytes.length, 1);
  const response = new Response(
    new ReadableStream<Uint8Array>({
      start(c) {
        controller = c;
        for (let i = 0; i < bytes.length; i += size) c.enqueue(bytes.slice(i, i + size));
        if (!opts.open) c.close();
      },
      cancel() {
        state.cancelled = true;
      },
    }),
  );
  return {
    response,
    state,
    push: (more: string | Uint8Array) => controller.enqueue(typeof more === "string" ? encoder.encode(more) : more),
    end: () => controller.close(),
  };
}

type Step = (url: string, signal: AbortSignal) => Response | Promise<Response>;
/** Answer the n-th request with the n-th step; a request beyond the script fails like the network. */
function scripted(...steps: Step[]) {
  const urls: string[] = [];
  const fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    urls.push(String(input));
    const step = steps[urls.length - 1];
    if (!step) throw new TypeError("offline");
    return step(String(input), init!.signal!);
  }) as typeof globalThis.fetch;
  return { fetch, urls };
}
const isWatch = (url: string) => new URL(url, "http://host").searchParams.get("watch") === "1";
/** The resourceVersion a WATCH asked to start from. */
const rvOf = (url: string) => new URL(url, "http://host").searchParams.get("resourceVersion");
/** Each request as `list` or `watch@<resourceVersion>`, so a test reads which reconnects resumed. */
const requested = (urls: string[]) => urls.map((u) => (isWatch(u) ? `watch@${rvOf(u)}` : "list"));
/** A step that never answers until the request is aborted, like a slow server. */
const hang: Step = (_url, signal) =>
  new Promise((_resolve, reject) => {
    signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
  });

function statuses(handle: ResourceStreamHandle): ConnectionStatus[] {
  const out: ConnectionStatus[] = [];
  handle.subscribe((state) => out.push(state.status));
  return out;
}
const until = (handle: ResourceStreamHandle, status: ConnectionStatus) =>
  new Promise<void>((resolve) => {
    const off = handle.subscribe((state) => {
      if (state.status === status) {
        off();
        resolve();
      }
    });
  });
const pendingTimeouts = () => process.getActiveResourcesInfo().filter((r) => r === "Timeout").length;

const url = "/k8s/api/v1/namespaces/app/configmaps?labelSelector=tier%3Dweb";

test("an empty collection is a complete snapshot, and the watch opens from its resourceVersion", async () => {
  const { fetch, urls } = scripted(
    () => collection([], "42"),
    () => body("", { open: true }).response,
  );
  const events: ResourceStateEvent[] = [];
  const handle = connectNativeWatch(url, (event) => events.push(event), { fetch });
  await until(handle, "live");
  handle.close();
  await handle.closed;
  assert.deepEqual(events, [{ type: "reset" }, { type: "synced" }]);
  assert.equal(urls[0], url, "the LIST is the collection URL, with no limit or continue");
  const watch = new URL(urls[1]!, "http://host");
  assert.equal(watch.pathname, "/k8s/api/v1/namespaces/app/configmaps");
  assert.equal(watch.searchParams.get("labelSelector"), "tier=web", "the WATCH uses the same selectors");
  assert.equal(watch.searchParams.get("watch"), "1");
  assert.equal(watch.searchParams.get("resourceVersion"), "42");
  assert.equal(watch.searchParams.get("allowWatchBookmarks"), "true", "bookmarks keep a quiet checkpoint fresh");
  assert.equal(watch.searchParams.has("limit"), false);
  assert.equal(watch.searchParams.has("sendInitialEvents"), false);
});

test("requests use same-origin credentials, no cache, cancellation and the caller's headers", async () => {
  const inits: RequestInit[] = [];
  const fetch = (async (_input: RequestInfo | URL, init?: RequestInit) => {
    inits.push(init!);
    return inits.length === 1 ? collection([]) : body("").response;
  }) as typeof globalThis.fetch;
  const handle = connectNativeWatch(url, ignore, { fetch, maxRetries: 0, headers: { "X-Host": "1" } });
  await handle.closed;
  assert.equal(inits.length, 2);
  for (const init of inits) {
    assert.equal(init.credentials, "same-origin");
    assert.equal(init.cache, "no-store");
    assert.ok(init.signal instanceof AbortSignal);
    assert.deepEqual(init.headers, { Accept: "application/json", "X-Host": "1" });
  }
});

test("the snapshot is applied, then the watch is accepted, and only then is the stream live", async () => {
  const log: string[] = [];
  let releaseWatch!: () => void;
  const watchOpened = new Promise<void>((resolve) => {
    releaseWatch = resolve;
  });
  const { fetch } = scripted(
    () => collection([cm("a", "one", "1"), cm("b", "two", "2")], "5"),
    async () => {
      log.push("watch requested");
      await watchOpened;
      log.push("watch accepted");
      return body("", { open: true }).response;
    },
  );
  const handle = connectNativeWatch(url, (event) => log.push(`apply ${event.type}`), { fetch });
  handle.subscribe((state) => {
    log.push(`state ${state.status}`);
    if (state.status === "live") handle.close();
  });
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(handle.state.status, "syncing", "still syncing while the watch is opening");
  releaseWatch();
  await handle.closed;
  assert.deepEqual(log, [
    "state connecting",
    "state syncing",
    "state syncing",
    "apply reset",
    "apply added",
    "apply added",
    "watch requested",
    "watch accepted",
    "apply synced",
    "state live",
    "state closed",
  ]);
});

test("the healthy period does not start while the watch is opening", async () => {
  // One failed attempt, then a watch that takes longer than the healthy period to be refused. Had
  // the timer started at the snapshot, the budget would be replenished and a third attempt made.
  const { fetch, urls } = scripted(
    () => json(status(503, "busy"), 503),
    () => collection([]),
    async (u) => {
      assert.ok(isWatch(u));
      await new Promise((resolve) => setTimeout(resolve, 30));
      return json(status(503, "busy"), 503);
    },
  );
  const states: Readonly<ConnectionState>[] = [];
  const handle = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, maxRetries: 2, healthyResetMs: 5 });
  handle.subscribe((state) => states.push(state));
  await handle.closed;
  assert.equal(urls.length, 4, "two retries, then the scripted network failure ends the budget");
  assert.ok(!states.some((s) => s.status === "live"));
  assert.deepEqual(
    states.filter((s) => s.status === "retrying").map((s) => s.retries),
    [1, 2],
  );
  assert.equal(handle.state.status, "exhausted");
});

test("missing item type metadata comes from a typed collection; present metadata and fields are kept", async () => {
  const bare = { metadata: { uid: "a", name: "one", resourceVersion: "1" }, data: { k: "v" }, extra: [1] };
  const own = { apiVersion: "example.com/v2", kind: "Override", metadata: { uid: "b", name: "two" } };
  const half = { apiVersion: "v1", metadata: { uid: "c", name: "three" } };
  const { fetch } = scripted(
    () => collection([bare, own, half], "9"),
    () =>
      body(
        frames({ type: "MODIFIED", object: { metadata: { uid: "a", name: "one", resourceVersion: "11" }, data: {} } }),
        { open: true },
      ).response,
  );
  const events: ResourceStateEvent[] = [];
  const handle = connectNativeWatch(url, (event) => events.push(event), { fetch });
  await until(handle, "live");
  await new Promise((resolve) => setTimeout(resolve, 5));
  handle.close();
  await handle.closed;
  const objects = events.flatMap((e) => ("object" in e ? [e.object] : []));
  assert.deepEqual(objects[0], { ...bare, apiVersion: "v1", kind: "ConfigMap" });
  assert.deepEqual(objects[1], own, "type metadata already present is never replaced");
  assert.deepEqual(objects[2], { ...half, kind: "ConfigMap" });
  assert.equal(objects[3]!.kind, "ConfigMap", "a watch object lacking its type gets the collection's item type");
  assert.ok(!objects.some((o) => o.kind === "ConfigMapList"), "never the collection's own kind");
});

for (const [label, list] of [
  ["a generic List", { kind: "List", apiVersion: "v1" }],
  ["a meta.k8s.io collection", { kind: "PartialObjectMetadataList", apiVersion: "meta.k8s.io/v1" }],
] as const)
  test(`${label} does not name its items' type, so an untyped item is malformed`, async () => {
    const errors: unknown[][] = [];
    const events: ResourceStateEvent[] = [];
    const { fetch, urls } = scripted(() =>
      collection([{ metadata: { uid: "a", name: "one" } }], "1", { kind: list.kind, apiVersion: list.apiVersion }),
    );
    const handle = connectNativeWatch("/k8s/api/v1/configmaps", (event) => events.push(event), {
      fetch,
      maxRetries: 1,
      retryDelayMs: 0,
      onError: (...args) => errors.push(args),
    });
    await handle.closed;
    assert.deepEqual(events, [], "nothing from a collection it cannot type");
    assert.equal(urls.length, 2, "a malformed collection is retried within the budget");
    assert.equal(errors[0]![0], "INTERNAL");
    assert.match(String(errors[0]![1]), /native list: .*no apiVersion or kind/);
    assert.equal(errors[0]![2], false);
    assert.equal(handle.state.status, "exhausted");
  });

for (const [label, items, pattern] of [
  ["an item without a uid", [{ apiVersion: "v1", kind: "ConfigMap", metadata: { name: "x" } }], /no metadata\.uid/],
  ["an item without a name", [{ apiVersion: "v1", kind: "ConfigMap", metadata: { uid: "x" } }], /no metadata\.name/],
  ["a uid listed twice", [cm("a", "one", "1"), cm("a", "two", "2")], /UID a twice/],
  ["an item that is not an object", ["nope"], /no metadata/],
] as const)
  test(`${label} rejects the whole collection before any reset`, async () => {
    const errors: unknown[][] = [];
    const events: ResourceStateEvent[] = [];
    const { fetch } = scripted(() => collection([...items]));
    const handle = connectNativeWatch(url, (event) => events.push(event), {
      fetch,
      maxRetries: 0,
      onError: (...args) => errors.push(args),
    });
    await handle.closed;
    assert.deepEqual(events, []);
    assert.equal(errors.length, 1);
    assert.match(String(errors[0]![1]), pattern);
  });

for (const [label, response, pattern] of [
  ["no resourceVersion", json({ kind: "ConfigMapList", apiVersion: "v1", metadata: {}, items: [] }), /resourceVersion/],
  ["no items", json({ kind: "ConfigMapList", apiVersion: "v1", metadata: { resourceVersion: "1" } }), /items/],
  ["a body that is not JSON", new Response("<html>login</html>"), /not JSON/],
] as const)
  test(`a collection with ${label} is malformed and retried`, async () => {
    const errors: unknown[][] = [];
    const { fetch } = scripted(() => response);
    const handle = connectNativeWatch(url, ignore, { fetch, maxRetries: 0, onError: (...args) => errors.push(args) });
    await handle.closed;
    assert.match(String(errors[0]![1]), pattern);
    assert.equal(errors[0]![2], false);
    assert.equal(handle.state.status, "exhausted");
  });

test("a paginated collection is refused, terminally, before reset: a page never establishes membership", async () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  store.applyServerEvent(cm("old", "kept", "1"));
  const errors: unknown[][] = [];
  const { fetch, urls } = scripted(() => collection([cm("a", "one", "2")], "5", { continue: "token" }));
  const handle = connectNativeWatch(url, (event) => applyStreamEvent(store, event), {
    fetch,
    retryDelayMs: 0,
    onError: (...args) => errors.push(args),
  });
  const states = statuses(handle);
  await handle.closed;
  assert.equal(urls.length, 1, "never retried and never watched");
  assert.ok(!urls[0]!.includes("limit") && !urls[0]!.includes("continue"), "no page was requested");
  assert.deepEqual(store.ids(), ["old"], "nothing applied and nothing pruned");
  assert.deepEqual(states, ["connecting", "syncing", "terminal"]);
  assert.equal(errors.length, 1);
  assert.equal(errors[0]![0], "INTERNAL");
  assert.match(String(errors[0]![1]), /paginated/);
  assert.equal(errors[0]![2], true);
});

test("watch frames split anywhere — across lines, JSON and UTF-8 — and many per chunk apply in order", async () => {
  const text = frames(
    { type: "ADDED", object: cm("b", "two", "11", "héllo ✓ 🚀") },
    { type: "MODIFIED", object: cm("a", "one", "12", "ünïcödé") },
    { type: "BOOKMARK", object: { kind: "ConfigMap", apiVersion: "v1", metadata: { resourceVersion: "13" } } },
    { type: "DELETED", object: cm("b", "two", "14", "héllo ✓ 🚀") },
  );
  for (const chunk of [1, 3, 7, 1_000_000]) {
    const store = new LiveResourceStore(readOnlyPolicy);
    const types: string[] = [];
    const watch = body(text, { chunk, open: true });
    const { fetch } = scripted(
      () => collection([cm("a", "one", "10")], "10"),
      () => watch.response,
    );
    const handle = connectNativeWatch(
      url,
      (event) => {
        types.push(event.type);
        applyStreamEvent(store, event);
        if (event.type === "deleted") handle.close();
      },
      { fetch },
    );
    await handle.closed;
    assert.deepEqual(types, ["reset", "added", "synced", "added", "modified", "deleted"], `chunk ${chunk}`);
    assert.deepEqual(store.ids(), ["a"]);
    assert.deepEqual(store.server("a").data, { value: "ünïcödé" }, `chunk ${chunk}`);
    assert.equal(store.server("a").metadata.resourceVersion, "12", "a bookmark changes no object");
    assert.equal(watch.state.cancelled, true);
  }
});

test("a deletion carries the identity of the deleted object", async () => {
  const events: ResourceStateEvent[] = [];
  const clusterScoped = { apiVersion: "v1", kind: "Namespace", metadata: { uid: "ns", name: "app" } };
  const { fetch } = scripted(
    () => collection([cm("a", "one", "1"), clusterScoped], "2", { kind: "List" }),
    () =>
      body(frames({ type: "DELETED", object: cm("a", "one", "3") }, { type: "DELETED", object: clusterScoped }))
        .response,
  );
  const handle = connectNativeWatch(url, (event) => events.push(event), { fetch, maxRetries: 0 });
  await handle.closed;
  assert.deepEqual(
    events.filter((e) => e.type === "deleted"),
    [
      {
        type: "deleted",
        identity: { uid: "a", apiVersion: "v1", kind: "ConfigMap", namespace: "app", name: "one" },
      },
      { type: "deleted", identity: { uid: "ns", apiVersion: "v1", kind: "Namespace", name: "app" } },
    ],
  );
});

test("a same-name recreation is a different resource: the old UID leaves and the new one arrives", async () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch } = scripted(
    () => collection([cm("old", "same", "1", "first")]),
    () =>
      body(
        frames(
          { type: "DELETED", object: cm("old", "same", "2", "first") },
          { type: "ADDED", object: cm("new", "same", "3", "second") },
        ),
      ).response,
  );
  const handle = connectNativeWatch(url, (event) => applyStreamEvent(store, event), { fetch, maxRetries: 0 });
  await handle.closed;
  assert.deepEqual(store.ids(), ["new"]);
  assert.deepEqual(store.server("new").data, { value: "second" });
});

for (const [label, tail, pattern] of [
  ["a frame that is not JSON", "{not json}\n", /not JSON/],
  ["a frame without an object", `${JSON.stringify({ type: "ADDED" })}\n`, /no type or object/],
  [
    "an unknown event type",
    `${JSON.stringify({ type: "RENAMED", object: cm("x", "x", "1") })}\n`,
    /unknown watch event/,
  ],
  ["an object without a uid", `${JSON.stringify({ type: "ADDED", object: { metadata: { name: "x" } } })}\n`, /uid/],
  ["invalid UTF-8", new Uint8Array([0xff, 0xfe, 0x0a]), /UTF-8/],
] as const)
  test(`${label} is reported, never skipped, and recovered by a fresh list rather than a resume`, async () => {
    const errors: unknown[][] = [];
    const types: string[] = [];
    const store = new LiveResourceStore(readOnlyPolicy);
    const first = body(frames({ type: "MODIFIED", object: cm("a", "one", "2", "before") }), { open: true });
    first.push(tail);
    first.end();
    const { fetch, urls } = scripted(
      () => collection([cm("a", "one", "1")]),
      () => first.response,
      () => collection([cm("a", "one", "3", "relisted")]),
      () => body("", { open: true }).response,
    );
    const handle = connectNativeWatch(
      url,
      (event) => {
        types.push(event.type);
        applyStreamEvent(store, event);
      },
      { fetch, retryDelayMs: 0, onError: (...args) => errors.push(args) },
    );
    const states = statuses(handle);
    let lives = 0;
    handle.subscribe((state) => {
      if (state.status === "live" && ++lives === 2) handle.close();
    });
    await handle.closed;
    assert.equal(urls.length, 4);
    assert.deepEqual(types, ["reset", "added", "synced", "modified", "reset", "added", "synced"]);
    assert.deepEqual(store.server("a").data, { value: "relisted" });
    assert.equal(errors.length, 1);
    assert.equal(errors[0]![0], "INTERNAL");
    assert.match(String(errors[0]![1]), /^native watch: /);
    assert.match(String(errors[0]![1]), pattern);
    assert.equal(errors[0]![2], false);
    assert.ok(states.includes("retrying"));
  });

test("HTTP 410 on the watch and an in-stream 410 both recover with a fresh snapshot", async () => {
  const errors: unknown[][] = [];
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1"), cm("b", "two", "1")], "1"),
    () => json(status(410, "too old resource version: 1 (7)", { reason: "Expired" }), 410),
    () => collection([cm("a", "one", "7")], "7"),
    () =>
      body(frames({ type: "ERROR", object: status(410, "too old resource version: 7 (9)", { reason: "Gone" }) }))
        .response,
    () => collection([cm("a", "one", "9")], "9"),
    () => body("", { open: true }).response,
  );
  const handle = connectNativeWatch(url, (event) => applyStreamEvent(store, event), {
    fetch,
    retryDelayMs: 0,
    onError: (...args) => errors.push(args),
  });
  const states = statuses(handle);
  let lives = 0;
  handle.subscribe((state) => {
    if (state.status === "live" && ++lives === 2) handle.close();
  });
  await handle.closed;
  assert.equal(urls.length, 6);
  assert.deepEqual(errors, [
    ["RESYNC_REQUIRED", "too old resource version: 1 (7)", false, undefined],
    ["RESYNC_REQUIRED", "too old resource version: 7 (9)", false, undefined],
  ]);
  assert.deepEqual(store.ids(), ["a"], "b was pruned by the completed replacement snapshot");
  assert.equal(store.server("a").metadata.resourceVersion, "9");
  assert.deepEqual(new URL(urls[3]!, "http://host").searchParams.get("resourceVersion"), "7");
  assert.equal(states.filter((s) => s === "retrying").length, 2);
});

test("repeated expiry consumes the bounded budget with backoff instead of looping on fresh lists", async () => {
  let lists = 0;
  const waits: number[] = [];
  const fetch = (async (input: RequestInfo | URL) => {
    if (!isWatch(String(input))) {
      lists++;
      return collection([], String(lists));
    }
    return json(status(410, "expired", { reason: "Expired" }), 410);
  }) as typeof globalThis.fetch;
  const handle = connectNativeWatch(url, ignore, { fetch, maxRetries: 3, retryDelayMs: 1, maxRetryDelayMs: 4 });
  handle.subscribe((state) => {
    if (state.status === "retrying") waits.push(state.retryInMs!);
  });
  await handle.closed;
  assert.equal(lists, 4, "one list per attempt, and the budget ends it");
  assert.equal(waits.length, 3);
  assert.equal(handle.state.status, "exhausted");
});

test("short EOFs on an accepted watch also exhaust the budget, resuming rather than re-listing", async () => {
  const requests: string[] = [];
  const fetch = (async (input: RequestInfo | URL) => {
    requests.push(isWatch(String(input)) ? "watch" : "list");
    return isWatch(String(input)) ? body("").response : collection([]);
  }) as typeof globalThis.fetch;
  const handle = connectNativeWatch(url, ignore, { fetch, maxRetries: 2, retryDelayMs: 0, healthyResetMs: 60_000 });
  const states = statuses(handle);
  await handle.closed;
  assert.deepEqual(requests, ["list", "watch", "watch", "watch"]);
  assert.equal(states.filter((s) => s === "live").length, 3);
  assert.equal(handle.state.status, "exhausted");
});

for (const [phase, code, expected] of [
  ["list", 401, "UNAUTHENTICATED"],
  ["list", 403, "FORBIDDEN"],
  ["watch", 401, "UNAUTHENTICATED"],
  ["watch", 403, "FORBIDDEN"],
  ["list", 404, "INTERNAL"],
  ["watch", 400, "INTERNAL"],
] as const)
  test(`HTTP ${code} on the ${phase} is terminal and keeps the Status message`, async () => {
    const errors: unknown[][] = [];
    const refusal = () => json(status(code, `${phase} refused with ${code}`), code);
    const { fetch, urls } = scripted(...(phase === "list" ? [refusal] : [() => collection([]), refusal]));
    const handle = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, onError: (...args) => errors.push(args) });
    await handle.closed;
    assert.equal(urls.length, phase === "list" ? 1 : 2, "never retried");
    assert.deepEqual(errors, [[expected, `${phase} refused with ${code}`, true, undefined]]);
    assert.equal(handle.state.status, "terminal");
  });

test("an in-stream 403 is terminal and nothing after it is applied", async () => {
  const errors: unknown[][] = [];
  const types: string[] = [];
  const { fetch, urls } = scripted(
    () => collection([]),
    () =>
      body(
        frames(
          { type: "ERROR", object: status(403, "access revoked", { reason: "Forbidden" }) },
          { type: "ADDED", object: cm("a", "one", "2") },
        ),
        { open: true },
      ).response,
  );
  const handle = connectNativeWatch(url, (event) => types.push(event.type), {
    fetch,
    onError: (...args) => errors.push(args),
  });
  await handle.closed;
  assert.equal(urls.length, 2);
  assert.deepEqual(types, ["reset", "synced"]);
  assert.deepEqual(errors, [["FORBIDDEN", "access revoked", true, undefined]]);
  assert.equal(handle.state.status, "terminal");
});

test("a refusal without a Status body is still classified, with a native message", async () => {
  const errors: unknown[][] = [];
  const { fetch } = scripted(() => new Response("denied", { status: 403 }));
  const handle = connectNativeWatch(url, ignore, { fetch, onError: (...args) => errors.push(args) });
  await handle.closed;
  assert.deepEqual(errors, [["FORBIDDEN", "native list: HTTP 403", true, undefined]]);
});

for (const code of [408, 429, 500, 503])
  test(`HTTP ${code} is retried`, async () => {
    const errors: unknown[][] = [];
    const { fetch, urls } = scripted(
      () => json(status(code, "try later"), code),
      () => collection([]),
      () => json(status(code, "try later"), code),
    );
    const handle = connectNativeWatch(url, ignore, {
      fetch,
      retryDelayMs: 0,
      maxRetries: 2,
      onError: (...args) => errors.push(args),
    });
    await handle.closed;
    assert.equal(urls.length, 4, "list, list + watch, then the scripted network failure");
    assert.equal(errors.length, 2);
    assert.ok(errors.every((e) => e[2] === false));
    assert.equal(handle.state.status, "exhausted");
  });

test("Retry-After and a Status's retryAfterSeconds set the least the reconnect waits", async () => {
  const cases: [Step[], unknown[]][] = [
    [
      [() => json(status(429, "slow down"), 429, { "Retry-After": "2" })],
      ["UPSTREAM_UNAVAILABLE", "slow down", false, 2000],
    ],
    [
      [
        () => collection([]),
        () =>
          body(
            frames({
              type: "ERROR",
              object: status(500, "etcd leader changed", {
                reason: "InternalError",
                details: { retryAfterSeconds: 3 },
              }),
            }),
          ).response,
      ],
      ["INTERNAL", "etcd leader changed", false, 3000],
    ],
  ];
  for (const [steps, error] of cases) {
    const errors: unknown[][] = [];
    const waits: number[] = [];
    const handle = connectNativeWatch(url, ignore, {
      fetch: scripted(...steps).fetch,
      retryDelayMs: 0,
      maxRetryDelayMs: 10_000,
      onError: (...args) => errors.push(args),
    });
    handle.subscribe((state) => {
      if (state.status !== "retrying") return;
      waits.push(state.retryInMs!);
      handle.close();
    });
    await handle.closed;
    assert.deepEqual(errors, [error]);
    assert.deepEqual(waits, [error[3]]);
  }
});

test("network failures on list, watch and body read consume exactly the retry budget", async () => {
  let calls = 0;
  const failing = new ReadableStream<Uint8Array>({
    pull(controller) {
      controller.error(new TypeError("connection reset"));
    },
  });
  const steps: Step[] = [
    () => {
      throw new TypeError("offline");
    },
    () => collection([]),
    () => {
      throw new TypeError("offline");
    },
    () => collection([]),
    () => new Response(failing),
  ];
  const errors: unknown[][] = [];
  const fetch = (async () => steps[calls++]!("", new AbortController().signal)) as typeof globalThis.fetch;
  const handle = connectNativeWatch(url, ignore, {
    fetch,
    retryDelayMs: 0,
    maxRetries: 2,
    onError: (...args) => errors.push(args),
  });
  await handle.closed;
  assert.equal(calls, 5);
  assert.deepEqual(errors, [], "a network failure is not malformed input");
  assert.equal(handle.state.status, "exhausted");
});

test("after expiry, missed deletes and selector exits are repaired by re-listing; prior state is kept until then", async () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch, urls } = scripted(
    // The first connection sees a, b and c; its watch then expires without reporting b's deletion
    // or c's label change taking it out of the selector, so there is nothing to resume from.
    () => collection([cm("a", "one", "1"), cm("b", "two", "1"), cm("c", "three", "1")], "1"),
    () =>
      body(frames({ type: "ERROR", object: status(410, "too old resource version", { reason: "Expired" }) })).response,
    // The replacement snapshot no longer lists them, but its watch is refused: incomplete, so the
    // next connection lists again rather than resuming from a snapshot that never completed.
    () => collection([cm("a", "one", "5")], "5"),
    () => json(status(503, "busy"), 503),
    () => collection([cm("a", "one", "6")], "6"),
    () => body("", { open: true }).response,
  );
  const seen: string[][] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      applyStreamEvent(store, event);
      if (event.type === "synced") seen.push(store.ids().sort());
    },
    { fetch, retryDelayMs: 0 },
  );
  handle.subscribe((state) => {
    if (state.status === "retrying") seen.push(store.ids().sort());
  });
  let lives = 0;
  handle.subscribe((state) => {
    if (state.status === "live" && ++lives === 2) handle.close();
  });
  await handle.closed;
  assert.deepEqual(seen, [["a", "b", "c"], ["a", "b", "c"], ["a", "b", "c"], ["a"]]);
  assert.equal(store.server("a").metadata.resourceVersion, "6");
  assert.deepEqual(requested(urls), ["list", "watch@1", "list", "watch@5", "list", "watch@6"]);
});

// ------------------------------------------------------------------------------------ resume --
//
// After a complete snapshot, a handle resumes its WATCH from the last event its consumer applied (or
// the last bookmark) instead of listing again. A resumed watch starts no snapshot: no LIST, no
// `reset`, no `synced`, and the state goes connecting → live on acceptance.

const bookmark = (rv: string) => ({
  type: "BOOKMARK",
  object: { kind: "ConfigMap", apiVersion: "v1", metadata: { resourceVersion: rv } },
});
const expired = (message = "too old resource version") => ({
  type: "ERROR",
  object: status(410, message, { reason: "Expired" }),
});
/** Close the handle the n-th time it goes live. */
function closeOnLive(handle: ResourceStreamHandle, n: number) {
  let lives = 0;
  handle.subscribe((state) => {
    if (state.status === "live" && ++lives === n) handle.close();
  });
}

test("an EOF resumes the watch from the last applied event and replays what was missed, without a snapshot", async () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch, urls } = scripted(
    () =>
      collection([cm("a", "one", "1"), cm("b", "two", "1"), cm("c", "three", "1"), cm("d", "same", "1", "first")], "1"),
    // The first watch delivers one change, then ends routinely.
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "2", "seen") })).response,
    // Meanwhile a changed, b was deleted, c's labels took it out of the selector and d was replaced
    // under the same name. The resumed watch replays all of it, in order.
    () =>
      body(
        frames(
          { type: "MODIFIED", object: cm("a", "one", "3", "missed") },
          { type: "DELETED", object: cm("b", "two", "4") },
          { type: "DELETED", object: cm("c", "three", "5") },
          { type: "DELETED", object: cm("d", "same", "6", "first") },
          { type: "ADDED", object: cm("d2", "same", "7", "second") },
        ),
        { open: true },
      ).response,
  );
  const types: string[] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      types.push(event.type);
      applyStreamEvent(store, event);
      if (event.type === "added" && event.object.metadata.uid === "d2") handle.close();
    },
    { fetch, retryDelayMs: 0 },
  );
  const states = statuses(handle);
  await handle.closed;

  assert.deepEqual(requested(urls), ["list", "watch@1", "watch@2"], "the reconnect resumed: no second LIST");
  const resumed = new URL(urls[2]!, "http://host");
  assert.equal(resumed.pathname, "/k8s/api/v1/namespaces/app/configmaps");
  assert.equal(resumed.searchParams.get("labelSelector"), "tier=web", "the same selectors");
  assert.equal(resumed.searchParams.get("allowWatchBookmarks"), "true");
  assert.deepEqual(types, [
    ...["reset", "added", "added", "added", "added", "synced", "modified"],
    ...["modified", "deleted", "deleted", "deleted", "added"],
  ]);
  assert.deepEqual(store.ids().sort(), ["a", "d2"]);
  assert.deepEqual(store.server("a").data, { value: "missed" });
  assert.equal(store.server("d2").metadata.name, "same");
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "live", "retrying", "connecting", "live", "closed"]);
});

test("a resumed watch is live only once accepted, and never syncing", async () => {
  let accept!: () => void;
  const accepted = new Promise<void>((resolve) => {
    accept = resolve;
  });
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1")], "1"),
    () => body("").response,
    async () => {
      await accepted;
      return body("", { open: true }).response;
    },
  );
  const types: string[] = [];
  const handle = connectNativeWatch(url, (event) => types.push(event.type), { fetch, retryDelayMs: 0 });
  const states = statuses(handle);
  closeOnLive(handle, 2);
  while (urls.length < 3) await new Promise((resolve) => setTimeout(resolve, 1));
  await new Promise((resolve) => setTimeout(resolve, 10));
  assert.equal(handle.state.status, "connecting", "not live while the resumed watch is opening");
  accept();
  await handle.closed;
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "live", "retrying", "connecting", "live", "closed"]);
  assert.deepEqual(types, ["reset", "added", "synced"], "acceptance alone delivers nothing");
});

test("a bookmark moves the checkpoint, and changes no object and reaches no consumer", async () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1")], "1"),
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "2", "changed") }, bookmark("9"))).response,
    // A quiet collection: nothing but a bookmark, and the checkpoint still moves.
    () => body(frames(bookmark("15"))).response,
    () => body("", { open: true }).response,
  );
  const types: string[] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      types.push(event.type);
      applyStreamEvent(store, event);
    },
    { fetch, retryDelayMs: 0 },
  );
  closeOnLive(handle, 3);
  await handle.closed;
  assert.deepEqual(requested(urls), ["list", "watch@1", "watch@9", "watch@15"]);
  assert.deepEqual(types, ["reset", "added", "synced", "modified"]);
  assert.equal(store.server("a").metadata.resourceVersion, "2", "a bookmark is never an object's version");
  assert.deepEqual(store.server("a").data, { value: "changed" });
});

test("resourceVersions are opaque: the last one in stream order is resumed from, whatever it looks like", async () => {
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "Zm9v/+=")], "Zm9v/+="),
    () =>
      body(
        frames(
          { type: "MODIFIED", object: cm("a", "one", "10") },
          // Smaller as a number and as a string, and still the latest: never compared.
          { type: "MODIFIED", object: cm("a", "one", "9") },
          { type: "ADDED", object: cm("b", "two", "ä b&c=d#e") },
        ),
      ).response,
    () => body("", { open: true }).response,
  );
  const handle = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0 });
  closeOnLive(handle, 2);
  await handle.closed;
  assert.deepEqual(requested(urls), ["list", "watch@Zm9v/+=", "watch@ä b&c=d#e"]);
});

for (const [label, tail, pattern] of [
  ["a frame", '{"type":"MODIFIED","object":{"metad', /ended inside a frame/],
  ["a character", new Uint8Array([0xe2, 0x9c]), /ended inside a UTF-8 character/],
] as const)
  test(`a watch that ends inside ${label} resumes from the last complete event`, async () => {
    const errors: unknown[][] = [];
    const store = new LiveResourceStore(readOnlyPolicy);
    // Frames split every 3 bytes, then cut off inside the next one.
    const first = body(frames({ type: "MODIFIED", object: cm("a", "one", "2", "complete ✓") }), {
      chunk: 3,
      open: true,
    });
    first.push(tail);
    first.end();
    const { fetch, urls } = scripted(
      () => collection([cm("a", "one", "1")], "1"),
      () => first.response,
      // The resumed watch's frames are split anywhere too, multi-byte characters included.
      () =>
        body(frames({ type: "MODIFIED", object: cm("a", "one", "3", "après 🚀") }), { chunk: 1, open: true }).response,
    );
    const types: string[] = [];
    const handle = connectNativeWatch(
      url,
      (event) => {
        types.push(event.type);
        applyStreamEvent(store, event);
        if (event.type === "modified" && event.object.metadata.resourceVersion === "3") handle.close();
      },
      { fetch, retryDelayMs: 0, onError: (...args) => errors.push(args) },
    );
    await handle.closed;
    assert.deepEqual(requested(urls), ["list", "watch@1", "watch@2"], "the partial frame was never applied");
    assert.deepEqual(types, ["reset", "added", "synced", "modified", "modified"]);
    assert.deepEqual(store.server("a").data, { value: "après 🚀" });
    assert.equal(errors.length, 1);
    assert.equal(errors[0]![0], "INTERNAL");
    assert.match(String(errors[0]![1]), pattern);
    assert.equal(errors[0]![2], false);
  });

for (const [label, frame, applied] of [
  ["a frame that is not JSON", "{not json}\n", []],
  ["an in-stream error without a code", frames({ type: "ERROR", object: { kind: "Status", message: "odd" } }), []],
  [
    "an event without a resourceVersion",
    frames({ type: "MODIFIED", object: { ...cm("a", "one", ""), metadata: { uid: "a", name: "one" } } }),
    ["modified"],
  ],
  ["a bookmark without a resourceVersion", frames({ type: "BOOKMARK", object: { kind: "ConfigMap" } }), []],
] as const)
  test(`${label} on a resumed watch discards the checkpoint: the next connection lists`, async () => {
    const { fetch, urls } = scripted(
      () => collection([cm("a", "one", "1")], "1"),
      () => body("").response,
      () => body(frame).response,
      () => collection([cm("a", "one", "5")], "5"),
      () => body("", { open: true }).response,
    );
    const types: string[] = [];
    const handle = connectNativeWatch(url, (event) => types.push(event.type), { fetch, retryDelayMs: 0 });
    closeOnLive(handle, 3);
    await handle.closed;
    assert.deepEqual(requested(urls), ["list", "watch@1", "watch@1", "list", "watch@5"]);
    assert.deepEqual(types, ["reset", "added", "synced", ...applied, "reset", "added", "synced"]);
  });

for (const [label, step] of [
  ["refused with 503", () => json(status(503, "busy"), 503)],
  [
    "lost to the network",
    () => {
      throw new TypeError("offline");
    },
  ],
  ["expired with HTTP 410", () => json(status(410, "too old resource version", { reason: "Expired" }), 410)],
] as [string, Step][])
  test(`an initialization whose watch is ${label} leaves no checkpoint: the next connection lists and prunes`, async () => {
    const store = new LiveResourceStore(readOnlyPolicy);
    store.applyServerEvent(cm("stale", "gone", "0"));
    const { fetch, urls } = scripted(
      () => collection([cm("a", "one", "1"), cm("b", "two", "1")], "1"),
      step,
      () => collection([cm("a", "one", "3")], "3"),
      () => body("", { open: true }).response,
    );
    const types: string[] = [];
    const handle = connectNativeWatch(
      url,
      (event) => {
        types.push(event.type);
        applyStreamEvent(store, event);
      },
      { fetch, retryDelayMs: 0 },
    );
    closeOnLive(handle, 1);
    await handle.closed;
    assert.deepEqual(requested(urls), ["list", "watch@1", "list", "watch@3"]);
    assert.deepEqual(types, ["reset", "added", "added", "reset", "added", "synced"]);
    assert.deepEqual(store.ids(), ["a"], "the completed snapshot pruned b and what preceded both");
  });

test("a consumer exception on a resumed watch closes the stream with it and requests nothing more", async () => {
  const boom = new Error("render failed");
  const resumed = body(
    frames({ type: "MODIFIED", object: cm("a", "one", "3") }, { type: "MODIFIED", object: cm("a", "one", "4") }),
    { open: true },
  );
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1")], "1"),
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "2") })).response,
    () => resumed.response,
  );
  const seen: string[] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      seen.push(event.type === "modified" ? `modified@${event.object.metadata.resourceVersion}` : event.type);
      if (event.type === "modified" && event.object.metadata.resourceVersion === "3") throw boom;
    },
    { fetch, retryDelayMs: 0 },
  );
  const states = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  await new Promise((resolve) => setTimeout(resolve, 5));
  assert.deepEqual(requested(urls), ["list", "watch@1", "watch@2"], "never retried");
  assert.deepEqual(seen, ["reset", "added", "synced", "modified@2", "modified@3"], "nothing after the exception");
  assert.equal(resumed.state.cancelled, true);
  assert.equal(states.at(-1), "closed");
});

test("HTTP 410 and an in-stream 410 on a resumed watch recover with a fresh, pruning snapshot", async () => {
  const errors: unknown[][] = [];
  const store = new LiveResourceStore(readOnlyPolicy);
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1"), cm("b", "two", "1")], "1"),
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "2") })).response,
    () => json(status(410, "too old resource version: 2 (7)", { reason: "Expired" }), 410),
    () => collection([cm("a", "one", "7")], "7"),
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "8") })).response,
    () => body(frames(expired("too old resource version: 8 (9)"))).response,
    () => collection([cm("a", "one", "9"), cm("c", "three", "9")], "9"),
    () => body("", { open: true }).response,
  );
  const types: string[] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      types.push(event.type);
      applyStreamEvent(store, event);
    },
    { fetch, retryDelayMs: 0, onError: (...args) => errors.push(args) },
  );
  const states = statuses(handle);
  closeOnLive(handle, 4);
  await handle.closed;
  assert.deepEqual(requested(urls), ["list", "watch@1", "watch@2", "list", "watch@7", "watch@8", "list", "watch@9"]);
  assert.deepEqual(errors, [
    ["RESYNC_REQUIRED", "too old resource version: 2 (7)", false, undefined],
    ["RESYNC_REQUIRED", "too old resource version: 8 (9)", false, undefined],
  ]);
  assert.deepEqual(types, [
    ...["reset", "added", "added", "synced", "modified"],
    ...["reset", "added", "synced", "modified"],
    ...["reset", "added", "added", "synced"],
  ]);
  assert.deepEqual(store.ids().sort(), ["a", "c"], "b was pruned by the completed replacement snapshot");
  // Never live from an expiry until the replacement snapshot completes.
  assert.deepEqual(states, [
    ...["connecting", "syncing", "syncing", "live", "retrying"],
    ...["connecting", "retrying"],
    ...["connecting", "syncing", "syncing", "live", "retrying"],
    ...["connecting", "live", "retrying"],
    ...["connecting", "syncing", "syncing", "live", "closed"],
  ]);
});

test("repeated expiry on resume consumes the bounded budget with backoff, never an immediate re-list", async () => {
  const log: string[] = [];
  let lists = 0;
  let initial = 0;
  const fetch = (async (input: RequestInfo | URL) => {
    const u = String(input);
    if (!isWatch(u)) {
      log.push("list");
      return collection([], String(++lists));
    }
    log.push(`watch@${rvOf(u)}`);
    // Every snapshot's watch passes one bookmark and ends; every resume from it has expired.
    return rvOf(u)!.startsWith("b")
      ? body(frames(expired())).response
      : body(frames(bookmark(`b${++initial}`))).response;
  }) as typeof globalThis.fetch;
  const errors: string[] = [];
  const handle = connectNativeWatch(url, ignore, {
    fetch,
    maxRetries: 3,
    retryDelayMs: 1,
    maxRetryDelayMs: 4,
    onError: (code) => errors.push(code),
  });
  handle.subscribe((state) => {
    if (state.status === "retrying" || state.status === "exhausted") log.push(state.status);
  });
  await handle.closed;
  assert.deepEqual(log, [
    ...["list", "watch@1", "retrying"],
    ...["watch@b1", "retrying"],
    ...["list", "watch@2", "retrying"],
    ...["watch@b2", "exhausted"],
  ]);
  assert.deepEqual(errors, ["RESYNC_REQUIRED", "RESYNC_REQUIRED"]);
  assert.equal(handle.state.status, "exhausted");
});

for (const [label, step, code] of [
  ["HTTP 403", () => json(status(403, "access revoked"), 403), "FORBIDDEN"],
  ["HTTP 401", () => json(status(401, "access revoked"), 401), "UNAUTHENTICATED"],
  [
    "an in-stream 403",
    () => body(frames({ type: "ERROR", object: status(403, "access revoked", { reason: "Forbidden" }) })).response,
    "FORBIDDEN",
  ],
] as [string, Step, string][])
  test(`${label} on a resumed watch is terminal`, async () => {
    const errors: unknown[][] = [];
    const { fetch, urls } = scripted(
      () => collection([], "1"),
      () => body("").response,
      step,
    );
    const handle = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, onError: (...args) => errors.push(args) });
    await handle.closed;
    assert.deepEqual(requested(urls), ["list", "watch@1", "watch@1"], "never retried or re-listed");
    assert.deepEqual(errors, [[code, "access revoked", true, undefined]]);
    assert.equal(handle.state.status, "terminal");
  });

test("transient failures on a resumed watch resume again from the same checkpoint", async () => {
  const errors: string[] = [];
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1")], "1"),
    () => body(frames({ type: "MODIFIED", object: cm("a", "one", "2") })).response,
    () => json(status(503, "busy"), 503),
    () => json(status(429, "slow down"), 429),
    () =>
      body(frames({ type: "ERROR", object: status(500, "etcd leader changed", { reason: "InternalError" }) })).response,
    () => body(frames({ type: "ERROR", object: status(504, "timeout", { reason: "Timeout" }) })).response,
    () => {
      throw new TypeError("offline");
    },
    () => body("").response,
    () => body("", { open: true }).response,
  );
  const types: string[] = [];
  const handle = connectNativeWatch(url, (event) => types.push(event.type), {
    fetch,
    retryDelayMs: 0,
    onError: (code) => errors.push(code),
  });
  closeOnLive(handle, 5);
  await handle.closed;
  assert.deepEqual(requested(urls), ["list", "watch@1", ...Array(7).fill("watch@2")]);
  assert.deepEqual(types, ["reset", "added", "synced", "modified"]);
  assert.deepEqual(errors, ["UPSTREAM_UNAVAILABLE", "UPSTREAM_UNAVAILABLE", "INTERNAL", "UPSTREAM_UNAVAILABLE"]);
});

test("close during a resumed watch's request, its body and the backoff before it", async (t) => {
  for (const at of ["resumed watch fetch", "resumed watch body", "backoff"] as const) {
    await t.test(at, async () => {
      const before = pendingTimeouts();
      const resumed = body("", { open: true });
      const steps: Step[] = [
        () => collection([cm("a", "one", "1")], "1"),
        () => body("").response,
        ...(at === "backoff" ? [] : [at === "resumed watch fetch" ? hang : () => resumed.response]),
      ];
      const { fetch, urls } = scripted(...steps);
      const events: string[] = [];
      const handle = connectNativeWatch(url, (event) => events.push(event.type), {
        fetch,
        retryDelayMs: at === "backoff" ? 60_000 : 0,
        healthyResetMs: 60_000,
      });
      const states = statuses(handle);
      if (at === "backoff") await until(handle, "retrying");
      else if (at === "resumed watch body") {
        while (states.filter((s) => s === "live").length < 2) await new Promise((resolve) => setTimeout(resolve, 1));
        resumed.push(frames({ type: "MODIFIED", object: cm("a", "one", "2") }));
      } else while (urls.length < 3) await new Promise((resolve) => setTimeout(resolve, 1));
      await new Promise((resolve) => setTimeout(resolve, 5));
      handle.close();
      await handle.closed;
      const count = events.length;
      await new Promise((resolve) => setTimeout(resolve, 5));
      assert.equal(events.length, count, "nothing is delivered after close");
      assert.equal(states.at(-1), "closed");
      assert.equal(urls.length, steps.length, "nothing is requested after close");
      assert.deepEqual(events, ["reset", "added", "synced", ...(at === "resumed watch body" ? ["modified"] : [])]);
      assert.equal(requested(urls).filter((r) => r === "list").length, 1, "the reconnect never listed");
      if (at === "resumed watch body") assert.equal(resumed.state.cancelled, true);
      if (at === "resumed watch fetch") assert.equal(states.filter((s) => s === "live").length, 1);
      assert.equal(pendingTimeouts(), before, "the backoff and health timers are released");
    });
  }
});

test("each handle keeps its own checkpoint, even on the same URL, and a new handle starts with a LIST", async () => {
  const requests: Record<string, string[]> = { a: [], b: [], c: [] };
  const scripts: Record<string, Step[]> = {
    a: [
      () => collection([cm("x", "one", "1")], "1"),
      () => body(frames({ type: "MODIFIED", object: cm("x", "one", "11") })).response,
      () => body("", { open: true }).response,
    ],
    b: [
      () => collection([cm("x", "one", "1")], "1"),
      () => body(frames({ type: "MODIFIED", object: cm("x", "one", "21") })).response,
      () => body("", { open: true }).response,
    ],
    c: [() => collection([cm("x", "one", "30")], "30"), () => body("", { open: true }).response],
  };
  // One fetch for every handle: the caller's header tells them apart, as a login identity would.
  const fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const who = (init!.headers as Record<string, string>)["X-Handle"]!;
    requests[who]!.push(String(input));
    return scripts[who]![requests[who]!.length - 1]!(String(input), init!.signal!);
  }) as typeof globalThis.fetch;
  const a = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, headers: { "X-Handle": "a" } });
  const b = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, headers: { "X-Handle": "b" } });
  closeOnLive(a, 2);
  closeOnLive(b, 2);
  await Promise.all([a.closed, b.closed]);
  const c = connectNativeWatch(url, ignore, { fetch, retryDelayMs: 0, headers: { "X-Handle": "c" } });
  closeOnLive(c, 1);
  await c.closed;
  assert.deepEqual(requested(requests.a!), ["list", "watch@1", "watch@11"]);
  assert.deepEqual(requested(requests.b!), ["list", "watch@1", "watch@21"]);
  assert.deepEqual(requested(requests.c!), ["list", "watch@30"], "a new handle never inherits a checkpoint");
});

test("drafts and conflicts survive a resumed watch, and a surviving draft survives the re-list after expiry", async () => {
  const store = new LiveResourceStore();
  const first = body("", { open: true });
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1", "base"), cm("b", "two", "1", "base")], "1"),
    () => first.response,
    // While disconnected, another writer changed a's edited field and an unrelated field of b.
    () =>
      body(
        frames(
          { type: "MODIFIED", object: cm("a", "one", "5", "theirs") },
          { type: "MODIFIED", object: { ...cm("b", "two", "6", "base"), data: { value: "base", other: "x" } } },
          expired(),
        ),
      ).response,
    // a was deleted after the history expired; b survives, with its unrelated change.
    () => collection([{ ...cm("b", "two", "7", "base"), data: { value: "base", other: "x" } }], "7"),
    () => body("", { open: true }).response,
  );
  const types: string[] = [];
  let afterResume: { conflicts: unknown; dirtyA: boolean; patchB: unknown } | undefined;
  const handle = connectNativeWatch(
    url,
    (event) => {
      types.push(event.type);
      applyStreamEvent(store, event);
    },
    { fetch, retryDelayMs: 0 },
  );
  handle.subscribe((state) => {
    if (state.status === "retrying" && urls.length === 3) {
      afterResume = {
        conflicts: store.conflicts("a"),
        dirtyA: store.isDirty("a", ["data", "value"]),
        patchB: store.patch("b"),
      };
    }
  });
  closeOnLive(handle, 3);
  await until(handle, "live");
  store.setValue("a", ["data", "value"], "mine");
  store.setValue("b", ["data", "value"], "mine too");
  first.end();
  await handle.closed;

  assert.deepEqual(requested(urls), ["list", "watch@1", "watch@1", "list", "watch@7"]);
  assert.deepEqual(types, [
    ...["reset", "added", "added", "synced"],
    ...["modified", "modified"],
    ...["reset", "added", "synced"],
  ]);
  assert.deepEqual(afterResume, {
    conflicts: [{ path: ["data", "value"], theirs: "theirs" }],
    dirtyA: true,
    patchB: { data: { value: "mine too" } },
  });
  assert.deepEqual(store.ids(), ["b"]);
  assert.equal(store.server("b").metadata.resourceVersion, "7");
  assert.deepEqual(store.draft("b").data, { value: "mine too", other: "x" });
  assert.deepEqual(store.patch("b"), { data: { value: "mine too" } });
});

test("close during the list request, the list body, the watch request, the watch body and backoff", async (t) => {
  for (const at of ["list fetch", "list body", "watch fetch", "watch body", "backoff"] as const) {
    await t.test(at, async () => {
      const before = pendingTimeouts();
      const listBody = body(JSON.stringify({ kind: "ConfigMapList", apiVersion: "v1" }).slice(0, 10), { open: true });
      const watchBody = body("", { open: true });
      const steps: Step[] =
        at === "list fetch"
          ? [hang]
          : at === "list body"
            ? [() => listBody.response]
            : at === "backoff"
              ? [() => json(status(503, "busy"), 503)]
              : [() => collection([cm("a", "one", "1")]), at === "watch fetch" ? hang : () => watchBody.response];
      const { fetch, urls } = scripted(...steps);
      const events: string[] = [];
      const handle = connectNativeWatch(url, (event) => events.push(event.type), {
        fetch,
        retryDelayMs: 60_000,
        healthyResetMs: 60_000,
      });
      const states = statuses(handle);
      const trigger: ConnectionStatus =
        at === "backoff" ? "retrying" : at === "watch body" ? "live" : at === "list fetch" ? "connecting" : "syncing";
      await until(handle, trigger);
      // Let the request or read get under way, so close() lands while it is pending.
      if (at === "watch body") watchBody.push(frames({ type: "ADDED", object: cm("b", "two", "2") }));
      await new Promise((resolve) => setTimeout(resolve, 5));
      handle.close();
      await handle.closed;
      const count = events.length;
      if (at === "watch body") {
        // The connector cancelled the body, so a late frame cannot even be enqueued.
        assert.throws(() => watchBody.push(frames({ type: "MODIFIED", object: cm("b", "two", "3") })));
      }
      await new Promise((resolve) => setTimeout(resolve, 5));
      assert.equal(events.length, count, "nothing is delivered after close");
      assert.equal(states.at(-1), "closed");
      assert.equal(states.filter((s) => s === "live").length, at === "watch body" ? 1 : 0);
      assert.equal(urls.length, steps.length, "nothing is requested after close");
      if (at === "list body") assert.equal(listBody.state.cancelled, true);
      if (at === "watch body") {
        assert.equal(watchBody.state.cancelled, true);
        assert.deepEqual(events, ["reset", "added", "synced", "added"]);
      }
      if (at === "watch fetch") assert.deepEqual(events, ["reset", "added"], "no synced without an accepted watch");
      assert.equal(pendingTimeouts(), before, "the backoff and health timers are released");
    });
  }
});

test("an already aborted signal never fetches; an external abort ends a live watch", async () => {
  const { fetch, urls } = scripted();
  const aborted = connectNativeWatch(url, ignore, { fetch, signal: AbortSignal.abort() });
  await aborted.closed;
  assert.equal(urls.length, 0);
  assert.equal(aborted.state.status, "closed");

  const controller = new AbortController();
  const watch = body("", { open: true });
  const live = connectNativeWatch(url, ignore, {
    signal: controller.signal,
    fetch: scripted(
      () => collection([]),
      () => watch.response,
    ).fetch,
  });
  await until(live, "live");
  controller.abort();
  await live.closed;
  assert.equal(watch.state.cancelled, true);
  assert.equal(live.state.status, "closed");
});

test("a consumer exception stops the stream, cancels the watch and rejects closed with it", async () => {
  const boom = new Error("render failed");
  const watch = body(
    frames({ type: "MODIFIED", object: cm("a", "one", "2") }, { type: "MODIFIED", object: cm("a", "one", "3") }),
    { open: true },
  );
  const { fetch, urls } = scripted(
    () => collection([cm("a", "one", "1")]),
    () => watch.response,
  );
  const seen: string[] = [];
  const errors: unknown[][] = [];
  const before = pendingTimeouts();
  const handle = connectNativeWatch(
    url,
    (event) => {
      seen.push(event.type);
      if (event.type === "modified") throw boom;
    },
    { fetch, retryDelayMs: 0, healthyResetMs: 60_000, onError: (...args) => errors.push(args) },
  );
  const states = statuses(handle);
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.equal(urls.length, 2, "never retried");
  assert.deepEqual(seen, ["reset", "added", "synced", "modified"]);
  assert.deepEqual(errors, []);
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "live", "closed"]);
  assert.equal(watch.state.cancelled, true);
  assert.equal(pendingTimeouts(), before);
});

test("a consumer exception during the snapshot never opens the watch", async () => {
  const boom = new Error("bad row");
  const { fetch, urls } = scripted(() => collection([cm("a", "one", "1"), cm("b", "two", "1")]));
  const seen: string[] = [];
  const handle = connectNativeWatch(
    url,
    (event) => {
      seen.push(event.type);
      if (event.type === "added") throw boom;
    },
    { fetch },
  );
  await assert.rejects(handle.closed, (error) => error === boom);
  assert.deepEqual(seen, ["reset", "added"]);
  assert.equal(urls.length, 1);
  assert.equal(handle.state.status, "closed");
});

test("a consumer that closes while applying synced gets no live", async () => {
  const watch = body("", { open: true });
  const { fetch } = scripted(
    () => collection([]),
    () => watch.response,
  );
  const handle = connectNativeWatch(
    url,
    (event) => {
      if (event.type === "synced") handle.close();
    },
    { fetch },
  );
  const states = statuses(handle);
  await handle.closed;
  assert.deepEqual(states, ["connecting", "syncing", "syncing", "closed"]);
  assert.equal(watch.state.cancelled, true);
});

test("a subscriber or onError that throws stops the stream without retrying", async () => {
  const boom = new Error("host bug");
  const subscriber = connectNativeWatch(url, ignore, {
    retryDelayMs: 0,
    fetch: scripted(
      () => collection([]),
      () => body("", { open: true }).response,
    ).fetch,
  });
  subscriber.subscribe((state) => {
    if (state.status === "live") throw boom;
  });
  await assert.rejects(subscriber.closed, (error) => error === boom);
  assert.equal(subscriber.state.status, "closed");

  const { fetch, urls } = scripted(() => json(status(503, "busy"), 503));
  const reporter = connectNativeWatch(url, ignore, {
    fetch,
    retryDelayMs: 0,
    onError: () => {
      throw boom;
    },
  });
  await assert.rejects(reporter.closed, (error) => error === boom);
  assert.equal(urls.length, 1);
  assert.equal(reporter.state.status, "closed");
});

test("the collection URL may not set what the connector sets", () => {
  for (const param of [
    "watch=1",
    "resourceVersion=5",
    "limit=10",
    "continue=x",
    "resourceVersionMatch=Exact",
    "sendInitialEvents=true",
    "allowWatchBookmarks=false",
  ]) {
    assert.throws(() => connectNativeWatch(`/k8s/api/v1/configmaps?${param}`, ignore), /must not set/, param);
  }
});

test("nativeCollectionURL addresses core and grouped, namespaced and cluster-scoped collections", () => {
  assert.equal(
    nativeCollectionURL("/k8s", { version: "v1", resource: "configmaps", namespace: "app" }),
    "/k8s/api/v1/namespaces/app/configmaps",
  );
  assert.equal(
    nativeCollectionURL("/k8s/", { group: "apps", version: "v1", resource: "deployments" }),
    "/k8s/apis/apps/v1/deployments",
  );
  assert.equal(
    nativeCollectionURL("", { group: "", version: "v1", resource: "namespaces", namespace: "" }),
    "/api/v1/namespaces",
  );
  assert.equal(
    nativeCollectionURL("https://host.example/proxy", {
      group: "widgets.example.com",
      version: "v1alpha1",
      resource: "widgets",
      namespace: "team-a",
      labelSelector: "app in (a,b),tier!=db",
      name: "w 1+2",
    }),
    "https://host.example/proxy/apis/widgets.example.com/v1alpha1/namespaces/team-a/widgets" +
      "?labelSelector=app+in+%28a%2Cb%29%2Ctier%21%3Ddb&fieldSelector=metadata.name%3Dw+1%2B2",
  );
  const query = new URL(
    nativeCollectionURL("/k8s", { version: "v1", resource: "pods", name: "web-0", labelSelector: "a=b" }),
    "http://host",
  ).searchParams;
  assert.equal(query.get("labelSelector"), "a=b");
  assert.equal(query.get("fieldSelector"), "metadata.name=web-0");
});

test("nativeCollectionURL refuses segments that would change the path", () => {
  for (const scope of [
    { version: "v1", resource: "" },
    { version: "", resource: "pods" },
    { version: "v1", resource: "pods", namespace: ".." },
    { version: "v1", resource: "pods", namespace: "a/b" },
    { group: ".", version: "v1", resource: "pods" },
    { version: "v1", resource: "../secrets" },
  ]) {
    assert.throws(() => nativeCollectionURL("/k8s", scope), /krm-stream: invalid/, JSON.stringify(scope));
  }
  assert.equal(
    nativeCollectionURL("/k8s", { version: "v1", resource: "pods", namespace: "a b" }),
    "/k8s/api/v1/namespaces/a%20b/pods",
  );
});

test("nativeObjectURL addresses one object in the collection a native watch reads", () => {
  assert.equal(
    nativeObjectURL("/k8s", { version: "v1", resource: "configmaps", namespace: "app", name: "settings" }),
    "/k8s/api/v1/namespaces/app/configmaps/settings",
  );
  assert.equal(
    nativeObjectURL("/k8s/", {
      group: "apps",
      version: "v1",
      resource: "deployments",
      name: "web",
      labelSelector: "a=b",
    }),
    "/k8s/apis/apps/v1/deployments/web",
    "a selector does not address an object",
  );
  assert.equal(
    nativeObjectURL("", { version: "v1", resource: "namespaces", name: "team a" }),
    "/api/v1/namespaces/team%20a",
  );
  for (const name of ["", ".", "..", "a/b"]) {
    assert.throws(
      () => nativeObjectURL("/k8s", { version: "v1", resource: "configmaps", namespace: "app", name }),
      /krm-stream: invalid name/,
      JSON.stringify(name),
    );
  }
});

test("nativeCollectionURL trims any run of trailing slashes from the proxy base in linear time", () => {
  assert.equal(nativeCollectionURL("/k8s///", { version: "v1", resource: "pods" }), "/k8s/api/v1/pods");
  assert.equal(nativeCollectionURL("///", { version: "v1", resource: "pods" }), "/api/v1/pods");
  // A base that is all slashes but for its last character backtracks quadratically under /\/+$/.
  const base = `${"/".repeat(100_000)}x`;
  const started = performance.now();
  assert.equal(nativeCollectionURL(base, { version: "v1", resource: "pods" }), `${base}/api/v1/pods`);
  assert.ok(performance.now() - started < 200, "trimming the proxy base must not backtrack");
});
