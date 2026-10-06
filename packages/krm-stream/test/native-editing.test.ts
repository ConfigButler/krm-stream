// Native editing: the store under the default policy, fed native objects, and the native conditional
// editor in examples/native-editor writing through a scripted host proxy. Each test answers the
// editor's PATCH and GET itself, so it decides what the API server said and when the watch moved.

import assert from "node:assert/strict";
import { test } from "node:test";
import { applyStreamEvent, connectNativeWatch, type KRMObject, LiveResourceStore } from "../src/index.ts";

const { nativeEditor, NativeRequestError } = await import("../../../examples/native-editor/editor.ts");

const LAST_APPLIED = "kubectl.kubernetes.io/last-applied-configuration";
const URL_ = "/k8s/api/v1/namespaces/app/configmaps/settings";
const source = { proxy: "/k8s", scope: { version: "v1", resource: "configmaps", namespace: "app" } };

/** A ConfigMap exactly as the API server returns it, machinery included. */
const native = (rv: string, value = "base", uid = "u"): KRMObject => ({
  apiVersion: "v1",
  kind: "ConfigMap",
  metadata: {
    uid,
    name: "settings",
    namespace: "app",
    resourceVersion: rv,
    annotations: { [LAST_APPLIED]: `{"data":{"value":"${value}"}}`, owner: "team-a" },
    managedFields: [{ manager: "kubectl", operation: "Apply" }],
  },
  data: { value },
});
const status = (code: number, reason: string, extra: Record<string, unknown> = {}) =>
  Response.json(
    { kind: "Status", apiVersion: "v1", status: "Failure", code, reason, message: reason, ...extra },
    {
      status: code,
    },
  );

interface Call {
  method: string;
  url: string;
  headers: Record<string, string>;
  body: unknown;
}

/** A host proxy answering each request with the next scripted response. */
function proxy(...answers: ((call: Call) => Response | Promise<Response>)[]) {
  const calls: Call[] = [];
  const request: typeof fetch = async (input, init) => {
    const call: Call = {
      method: init?.method ?? "GET",
      url: String(input),
      headers: (init?.headers ?? {}) as Record<string, string>,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    };
    calls.push(call);
    const answer = answers.shift();
    assert.ok(answer, `unexpected ${call.method} ${call.url}`);
    return answer(call);
  };
  return { calls, request };
}

function seeded(rv = "1"): LiveResourceStore {
  const store = new LiveResourceStore();
  store.applyServerEvent(native(rv));
  return store;
}

test("a native save is a conditional merge patch to the object, through the watched proxy", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "mine");
  store.setValue("u", ["metadata", "annotations", "owner"], "team-b");
  const { calls, request } = proxy(() => {
    store.setValue("u", ["data", "value"], "typed while saving");
    // The proxy answers with the written object, which the editor must not adopt.
    return Response.json(native("2", "mine"));
  });
  const editor = nativeEditor(store, "u", source, request, () => true);

  assert.equal(await editor.save(), "saved");
  assert.equal(calls.length, 1);
  assert.equal(calls[0]!.method, "PATCH");
  assert.equal(calls[0]!.url, URL_);
  assert.equal(calls[0]!.headers["Content-Type"], "application/merge-patch+json");
  assert.deepEqual(calls[0]!.body, {
    metadata: { annotations: { owner: "team-b" }, uid: "u", resourceVersion: "1" },
    data: { value: "mine" },
  });
  // The response was not adopted: the server object still waits for the watch.
  assert.equal(store.server("u").metadata.resourceVersion, "1");
  assert.deepEqual(store.draft("u").data, { value: "typed while saving" });

  // The echo arrives: the saved value settles, the later typing stays.
  store.applyServerEvent({
    ...native("2", "mine"),
    metadata: { ...native("2", "mine").metadata, annotations: { [LAST_APPLIED]: "{}", owner: "team-b" } },
  });
  assert.deepEqual(store.patch("u"), { data: { value: "typed while saving" } });
  assert.equal(editor.saving, false);
});

test("a native save never carries machinery, even when the server rewrote it during editing", async () => {
  const store = seeded();
  store.setValue("u", ["metadata", "annotations", "owner"], "team-b");
  assert.throws(() => store.removeKey("u", ["metadata", "annotations"]), /read-only/);
  store.applyServerEvent(native("2", "applied again"));
  const { calls, request } = proxy(() => new Response(null, { status: 200 }));
  assert.equal(await nativeEditor(store, "u", source, request, () => true).save(), "saved");
  assert.deepEqual(calls[0]!.body, {
    metadata: { annotations: { owner: "team-b" }, uid: "u", resourceVersion: "2" },
  });
});

test("a native 409 reconciles a guarded native GET and keeps the draft for review", async () => {
  const store = seeded();
  store.setValue("u", ["metadata", "annotations", "owner"], "team-b");
  const { calls, request } = proxy(
    () => status(409, "Conflict"),
    (call) => {
      assert.equal(call.url, URL_);
      store.setValue("u", ["metadata", "annotations", "owner"], "team-c");
      return Response.json(native("3", "moved on the server"));
    },
    () => new Response(null, { status: 200 }),
  );
  const editor = nativeEditor(store, "u", source, request, () => true);

  assert.equal(await editor.save(), "version-stale");
  assert.deepEqual(
    calls.map((c) => c.method),
    ["PATCH", "GET"],
  );
  assert.equal(store.server("u").metadata.resourceVersion, "3");
  assert.equal(store.draft("u").metadata.annotations?.owner, "team-c");
  assert.deepEqual(store.draft("u").data, { value: "moved on the server" });

  // The next deliberate Save captures a new intent at the version the read established.
  assert.equal(await editor.save(), "saved");
  assert.deepEqual(calls[2]!.body, {
    metadata: { annotations: { owner: "team-c" }, uid: "u", resourceVersion: "3" },
  });
});

test("a native 409 with a concurrent change to the edited field is a draft conflict", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "mine");
  const { request } = proxy(
    () => status(409, "Conflict"),
    () => Response.json(native("3", "theirs")),
  );
  const editor = nativeEditor(store, "u", source, request, () => true);
  assert.equal(await editor.save(), "draft-conflict");
  assert.deepEqual(store.conflicts("u"), [{ path: ["data", "value"], theirs: "theirs" }]);
  assert.equal(await editor.save(), "draft-conflict", "no write until the conflict is resolved");
});

test("an object replaced under the same name is unavailable, and its replacement is never applied", async () => {
  for (const refusal of [
    () => status(409, "Conflict"),
    () => status(422, "Invalid", { details: { causes: [{ field: "metadata.uid", message: "Precondition failed" }] } }),
  ]) {
    const store = seeded();
    store.setValue("u", ["data", "value"], "mine");
    const { calls, request } = proxy(refusal, () => Response.json(native("4", "replacement", "u-new")));
    assert.equal(await nativeEditor(store, "u", source, request, () => true).save(), "unavailable");
    assert.deepEqual(
      calls.map((c) => c.method),
      ["PATCH", "GET"],
    );
    assert.equal(store.server("u").metadata.resourceVersion, "1");
    assert.deepEqual(store.draft("u").data, { value: "mine" });
  }
});

test("a deleted object is unavailable on save and on the recovery read", async () => {
  const deleted = seeded();
  deleted.setValue("u", ["data", "value"], "mine");
  const patch = proxy(() => status(404, "NotFound"));
  assert.equal(await nativeEditor(deleted, "u", source, patch.request, () => true).save(), "unavailable");

  const gone = seeded();
  gone.setValue("u", ["data", "value"], "mine");
  const read = proxy(
    () => status(409, "Conflict"),
    () => status(404, "NotFound"),
  );
  assert.equal(await nativeEditor(gone, "u", source, read.request, () => true).save(), "unavailable");
});

test("a refused native write keeps the Kubernetes Status for the form and the draft intact", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "x".repeat(10));
  const invalid = { causes: [{ field: "data.value", message: "too long" }] };
  const { request } = proxy(() => status(422, "Invalid", { details: invalid }));
  const error = await nativeEditor(store, "u", source, request, () => true)
    .save()
    .then(
      () => undefined,
      (e: unknown) => e,
    );
  assert.ok(error instanceof NativeRequestError);
  assert.equal(error.httpStatus, 422);
  assert.deepEqual((error.status as { details: unknown }).details, invalid);
  assert.deepEqual(store.patch("u"), { data: { value: "xxxxxxxxxx" } });
});

test("a recovery read overtaken by the watch is not applied, and the next Save reads again", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "mine");
  const { calls, request } = proxy(
    () => status(409, "Conflict"),
    () => {
      store.applyServerEvent(native("5", "base"));
      return Response.json(native("4", "older"));
    },
    () => Response.json(native("5", "base")),
  );
  const editor = nativeEditor(store, "u", source, request, () => true);
  assert.equal(await editor.save(), "recovering");
  assert.equal(store.server("u").metadata.resourceVersion, "5");
  assert.equal(await editor.save(), "version-stale", "only a read, never the old patch");
  assert.deepEqual(
    calls.map((c) => c.method),
    ["PATCH", "GET", "GET"],
  );
});

test("the native editor refuses to write while the connection is not live, and rejects a projected envelope", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "mine");
  const idle = proxy();
  assert.equal(await nativeEditor(store, "u", source, idle.request, () => false).save(), "recovering");
  assert.equal(idle.calls.length, 0);

  // A projected endpoint's { object, redactedPaths } is another source; it never reconciles here.
  const projected = proxy(
    () => status(409, "Conflict"),
    () => Response.json({ object: native("2", "theirs"), redactedPaths: [] }),
  );
  await assert.rejects(
    nativeEditor(store, "u", source, projected.request, () => true).save(),
    /not a Kubernetes object/,
  );
  assert.equal(store.server("u").metadata.resourceVersion, "1");
});

test("a write whose outcome is unknown is followed by a guarded read, never a second write", async () => {
  // The network failed after sending, and the write landed.
  const lost = seeded();
  lost.setValue("u", ["data", "value"], "mine");
  const network = proxy(
    () => {
      throw new TypeError("network error");
    },
    () => Response.json(native("2", "mine")),
  );
  const lostEditor = nativeEditor(lost, "u", source, network.request, () => true);
  await assert.rejects(lostEditor.save(), /network error/);
  assert.equal(await lostEditor.save(), "confirmed");
  assert.equal(lost.patch("u"), null, "the read found the write had landed");
  assert.equal(await lostEditor.save(), "unchanged");
  assert.deepEqual(
    network.calls.map((c) => c.method),
    ["PATCH", "GET"],
  );

  // The proxy answered 502, and the write did not land: the read establishes that, and only the
  // next deliberate Save writes again.
  const failed = seeded();
  failed.setValue("u", ["data", "value"], "mine");
  const gateway = proxy(
    () => status(502, "BadGateway"),
    () => Response.json(native("1")),
    () => new Response(null, { status: 200 }),
  );
  const failedEditor = nativeEditor(failed, "u", source, gateway.request, () => true);
  await assert.rejects(failedEditor.save(), NativeRequestError);
  assert.equal(await failedEditor.save(), "confirmed");
  assert.deepEqual(failed.patch("u"), { data: { value: "mine" } });
  assert.equal(await failedEditor.save(), "saved");
  assert.deepEqual(
    gateway.calls.map((c) => c.method),
    ["PATCH", "GET", "PATCH"],
  );
});

test("a definite refusal needs no read: the corrected draft is written next", async () => {
  const store = seeded();
  store.setValue("u", ["data", "value"], "invalid");
  const { calls, request } = proxy(
    () => status(422, "Invalid", { details: { causes: [{ field: "data.value" }] } }),
    () => new Response(null, { status: 200 }),
  );
  const editor = nativeEditor(store, "u", source, request, () => true);
  await assert.rejects(editor.save(), NativeRequestError);
  store.setValue("u", ["data", "value"], "valid");
  assert.equal(await editor.save(), "saved");
  assert.deepEqual(
    calls.map((c) => c.method),
    ["PATCH", "PATCH"],
  );
});

test("an accepted write is confirmed by its echo, or by a guarded read when no echo arrives", async () => {
  // No echo: the next Save reads instead of writing again, and never adopts the write response.
  const quiet = seeded();
  quiet.setValue("u", ["data", "value"], "mine");
  const read = proxy(
    () => Response.json(native("2", "mine")),
    () => Response.json(native("2", "mine")),
  );
  const quietEditor = nativeEditor(quiet, "u", source, read.request, () => true);
  assert.equal(await quietEditor.save(), "saved");
  assert.equal(quiet.server("u").metadata.resourceVersion, "1", "the write response is not adopted");
  assert.equal(await quietEditor.save(), "confirmed");
  assert.equal(quiet.patch("u"), null);
  assert.equal(await quietEditor.save(), "unchanged");
  assert.deepEqual(
    read.calls.map((c) => c.method),
    ["PATCH", "GET"],
  );

  // The host confirms explicitly, as after a timeout; an admission webhook restored the value, so
  // the edit is still dirty and the next Save writes it again deliberately.
  const restored = seeded();
  restored.setValue("u", ["data", "value"], "mine");
  const confirm = proxy(
    () => new Response(null, { status: 200 }),
    () => Response.json(native("1")),
    () => new Response(null, { status: 200 }),
  );
  const restoredEditor = nativeEditor(restored, "u", source, confirm.request, () => true);
  assert.equal(await restoredEditor.save(), "saved");
  assert.equal(await restoredEditor.confirm(), "confirmed");
  assert.deepEqual(restored.patch("u"), { data: { value: "mine" } });
  assert.equal(await restoredEditor.save(), "saved");
  assert.deepEqual(
    confirm.calls.map((c) => c.method),
    ["PATCH", "GET", "PATCH"],
  );

  // The echo arrives: nothing is owed, and the next edit is written at the echoed version.
  const echoed = seeded();
  echoed.setValue("u", ["data", "value"], "mine");
  const write = proxy(
    () => new Response(null, { status: 200 }),
    () => new Response(null, { status: 200 }),
  );
  const echoedEditor = nativeEditor(echoed, "u", source, write.request, () => true);
  assert.equal(await echoedEditor.save(), "saved");
  echoed.applyServerEvent(native("2", "mine"));
  echoed.setValue("u", ["data", "value"], "again");
  assert.equal(await echoedEditor.save(), "saved");
  assert.deepEqual(
    write.calls.map((c) => [c.method, (c.body as { metadata: { resourceVersion: string } }).metadata.resourceVersion]),
    [
      ["PATCH", "1"],
      ["PATCH", "2"],
    ],
  );
});

test("saving is refused while a native watch recovers from expiry, and works again once a resume is live", async () => {
  // The watch side: a scripted proxy for LIST and WATCH, driving the real connector into the store.
  const encoder = new TextEncoder();
  const open = () => {
    let controller!: ReadableStreamDefaultController<Uint8Array>;
    const response = new Response(
      new ReadableStream<Uint8Array>({
        start: (c) => {
          controller = c;
        },
      }),
    );
    return {
      response,
      push: (text: string) => controller.enqueue(encoder.encode(text)),
      end: () => controller.close(),
    };
  };
  const frame = (type: string, object: unknown) => `${JSON.stringify({ type, object })}\n`;
  const list = (rv: string, ...items: KRMObject[]) =>
    Response.json({ kind: "ConfigMapList", apiVersion: "v1", metadata: { resourceVersion: rv }, items });
  const watches = [open(), open(), open()];
  let relist!: () => void;
  const relisted = new Promise<void>((resolve) => {
    relist = resolve;
  });
  const watchRequests: string[] = [];
  const steps: (() => Response | Promise<Response>)[] = [
    () => list("1", native("1")),
    () => watches[0]!.response,
    // The replacement snapshot after the expiry is held, so the test can save while it is incomplete.
    async () => {
      await relisted;
      return list("3", native("3", "base"));
    },
    () => watches[1]!.response,
    () => watches[2]!.response,
  ];
  const watchFetch = (async (input: RequestInfo | URL) => {
    watchRequests.push(String(input));
    return steps[watchRequests.length - 1]!();
  }) as typeof fetch;
  const store = new LiveResourceStore();
  const connection = connectNativeWatch(
    "/k8s/api/v1/namespaces/app/configmaps?fieldSelector=metadata.name%3Dsettings",
    (event) => applyStreamEvent(store, event),
    { fetch: watchFetch, retryDelayMs: 0 },
  );
  const states: string[] = [];
  connection.subscribe((state) => states.push(state.status));
  const reached = async (lives: number) => {
    while (states.filter((s) => s === "live").length < lives) await new Promise((resolve) => setTimeout(resolve, 1));
  };
  const live = () => connection.state.status === "live";
  await reached(1);

  store.setValue("u", ["data", "value"], "mine");
  const { calls, request } = proxy(() => new Response(null, { status: 200 }));
  const editor = nativeEditor(store, "u", source, request, live);

  // The history expires: until the replacement snapshot completes, no write is attempted.
  watches[0]!.push(frame("ERROR", { kind: "Status", code: 410, reason: "Expired", message: "too old" }));
  watches[0]!.end();
  while (watchRequests.length < 3) await new Promise((resolve) => setTimeout(resolve, 1));
  assert.notEqual(connection.state.status, "live");
  assert.equal(await editor.save(), "recovering");
  assert.equal(calls.length, 0);
  relist();
  await reached(2);
  assert.deepEqual(store.draft("u").data, { value: "mine" }, "the draft survived the re-list");
  assert.equal(store.server("u").metadata.resourceVersion, "3");

  // A routine EOF resumes from the checkpoint with no snapshot: the draft is untouched, and the save
  // goes out once the resumed watch is live, at the version the store holds.
  watches[1]!.end();
  await reached(3);
  assert.equal(watchRequests.length, 5, "no LIST for the resume");
  assert.equal(new URL(watchRequests[4]!, "http://host").searchParams.get("resourceVersion"), "3");
  assert.equal(await editor.save(), "saved");
  assert.deepEqual(calls[0]!.body, { metadata: { uid: "u", resourceVersion: "3" }, data: { value: "mine" } });
  connection.close();
  await connection.closed;
});
