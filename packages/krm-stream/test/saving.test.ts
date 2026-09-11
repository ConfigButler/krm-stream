import assert from "node:assert/strict";
import { test } from "node:test";
import { LiveResourceStore } from "../src/index.ts";

const object = (rv: string, value = "base") => ({
  apiVersion: "v1",
  kind: "ConfigMap",
  metadata: { uid: "u", name: "cm", resourceVersion: rv },
  data: { value },
});

test("save capture detaches patch and binds it to the merge base", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  assert.equal(store.captureSave("u"), null);
  store.setValue("u", ["data", "value"], "first");
  const save = store.captureSave("u");
  store.applyServerEvent(object("2"));
  store.setValue("u", ["data", "value"], "second");
  assert.deepEqual(save, { uid: "u", resourceVersion: "1", patch: { data: { value: "first" } } });
});

test("409 reconciliation preserves drafts and exposes conflicts", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  store.setValue("u", ["data", "value"], "mine");
  const reconcile = store.captureReconciliation("u");
  store.setValue("u", ["data", "value"], "newer draft");
  assert.equal(reconcile(object("2", "theirs")), true);
  assert.deepEqual(store.draft("u").data, { value: "newer draft" });
  assert.equal(store.conflicts("u").length, 1);
  assert.equal(reconcile(object("2", "theirs")), false);
});

test("late reads and saves cannot overwrite a newer watch or resurrect deletions", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  const read = store.captureReconciliation("u");
  const save = store.captureReconciliation("u");
  store.applyServerEvent(object("3", "watch"));
  assert.equal(read(object("2")), false);
  assert.equal(save(object("2")), false);
  const deleted = store.captureReconciliation("u");
  store.removeResource("u");
  assert.equal(deleted(object("4")), false);
  store.applyServerEvent(object("5"));
  assert.equal(deleted(object("4")), false);
});

test("a reconciliation response for a replacement UID is rejected", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  const reconcile = store.captureReconciliation("u");
  const replacement = object("2");
  replacement.metadata.uid = "replacement";
  assert.equal(reconcile(replacement), false);
  assert.deepEqual(store.ids(), ["u"]);
});

test("array edits capture the replacement array with its original version", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent({ ...object("1"), spec: { items: ["a", "b"] } });
  store.setValue("u", ["spec", "items", 0], "mine");
  const captured = store.captureSave("u");
  store.applyServerEvent({ ...object("2"), spec: { items: ["a", "theirs"] } });
  assert.deepEqual(captured, { uid: "u", resourceVersion: "1", patch: { spec: { items: ["mine", "b"] } } });
});

test("the complete editor reconciles an HTTP 409 and never retries the write blindly", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  store.setValue("u", ["data", "value"], "mine");
  const methods: string[] = [];
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    async (_url, init) => {
      methods.push(init?.method ?? "GET");
      if (init?.method === "PATCH") {
        assert.deepEqual(JSON.parse(init.body as string), {
          uid: "u",
          resourceVersion: "1",
          patch: { data: { value: "mine" } },
        });
        return new Response(null, { status: 409 });
      }
      store.setValue("u", ["data", "value"], "typed while saving");
      return Response.json({ object: object("2", "theirs"), redactedPaths: [] });
    },
    () => true,
  );
  assert.equal(await editor.save(), "draft-conflict");
  assert.deepEqual(methods, ["PATCH", "GET"]);
  assert.deepEqual(store.draft("u").data, { value: "typed while saving" });
  assert.equal(store.conflicts("u").length, 1);
  assert.equal(editor.saving, false);
});

test("the editor ignores a reconciliation GET overtaken by the watch", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  store.setValue("u", ["data", "value"], "mine");
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    async (_url, init) => {
      if (init?.method === "PATCH") return new Response(null, { status: 409 });
      store.applyServerEvent(object("3", "watch"));
      return Response.json({ object: object("2", "stale GET"), redactedPaths: [] });
    },
    () => true,
  );
  assert.equal(await editor.save(), "draft-conflict");
  assert.deepEqual(store.server("u").data, { value: "watch" });
});

test("a late GET cannot mark an object present in a recovery snapshot", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  const beforeReset = store.captureReconciliation("u");
  store.beginSnapshot();
  const duringReset = store.captureReconciliation("u");
  assert.equal(beforeReset(object("2")), false);
  assert.equal(duringReset(object("2")), false);
  store.endSnapshot();
  assert.deepEqual(store.ids(), []);
  assert.equal(beforeReset(object("2")), false);
});

test("stateless reconciliation preserves redaction protection without inventing revisions", () => {
  const store = new LiveResourceStore();
  const secret = { ...object("1"), kind: "Secret", data: {} };
  store.applyServerEvent(secret, { redacted: [{ path: "/data/token", rev: 7 }] });
  assert.equal(
    store.captureReconciliation("u")({ ...secret, metadata: { ...secret.metadata, resourceVersion: "2" } }),
    true,
  );
  assert.deepEqual(store.redactions("u"), [{ path: ["data", "token"], rev: 7 }]);
  assert.equal(store.isEditable("u", ["data", "token"]), false);
  const reconcile = store.captureReconciliation("u");
  assert.equal(reconcile(secret, { redactedPaths: ["/data/token", "/data/new"] }), false);
  assert.equal(store.server("u").metadata.resourceVersion, "2");
  assert.equal(reconcile(secret, { redactedPaths: ["/data/token"] }), true);
  assert.deepEqual(store.redactions("u"), [{ path: ["data", "token"], rev: 7 }]);
  assert.equal(store.captureReconciliation("u")(secret, { redactedPaths: [] }), true);
  assert.deepEqual(store.redactions("u"), []);
});

test("save after deletion returns unavailable without making a request", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    async () => {
      assert.fail("request after deletion");
    },
    () => true,
  );
  store.removeResource("u");
  assert.equal(await editor.save(), "unavailable");
});

test("editor distinguishes version rejection and captures a fresh intent only on the next save", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  let patches = 0;
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    async (_url, init) => {
      if (init?.method !== "PATCH") return Response.json({ object: object("2"), redactedPaths: [] });
      patches++;
      assert.equal(JSON.parse(init.body as string).resourceVersion, patches === 1 ? "1" : "2");
      if (patches === 1) return new Response(null, { status: 409 });
      store.setValue("u", ["data", "value"], "typed during write");
      return new Response(null, { status: 204 });
    },
    () => true,
  );
  assert.equal(await editor.save(), "unchanged");
  store.setValue("u", ["data", "value"], "mine");
  assert.equal(await editor.save(), "version-stale");
  assert.equal(patches, 1);
  assert.equal(store.conflicts("u").length, 0);
  assert.equal(await editor.save(), "saved");
  assert.deepEqual(store.draft("u").data, { value: "typed during write" });
});

test("editor waits for live recovery and never uses a refused GET as snapshot membership", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  for (const finishBeforeResponse of [false, true]) {
    const store = new LiveResourceStore();
    store.applyServerEvent(object("1"));
    store.setValue("u", ["data", "value"], "mine");
    let live = true;
    let reads = 0;
    let patches = 0;
    const editor = conditionalEditor(
      store,
      "u",
      "/save",
      async (_url, init) => {
        if (init?.method === "PATCH") {
          patches++;
          return new Response(null, { status: 409 });
        }
        if (++reads === 1) {
          store.beginSnapshot();
          live = false;
          if (finishBeforeResponse) {
            store.applyServerEvent(object("3"));
            store.endSnapshot();
            live = true;
          }
        }
        return Response.json({ object: object(reads === 1 ? "2" : "3"), redactedPaths: [] });
      },
      () => live,
    );
    assert.equal(await editor.save(), "recovering");
    assert.equal(store.server("u").metadata.resourceVersion, finishBeforeResponse ? "3" : "1");
    if (!finishBeforeResponse) {
      assert.equal(await editor.save(), "recovering");
      assert.equal(reads, 1);
      store.applyServerEvent(object("3"));
      store.endSnapshot();
      live = true;
    }
    assert.equal(await editor.save(), "version-stale");
    assert.equal(patches, 1); // Recovery click only reads, never retries the write.
    assert.deepEqual(store.draft("u").data, { value: "mine" });
  }
});

test("editor reports unavailable for missing or replacement reads and preserves host errors", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  for (const status of [404, 200, 403]) {
    const store = new LiveResourceStore();
    store.applyServerEvent(object("1"));
    store.setValue("u", ["data", "value"], "mine");
    const replacement = object("2");
    replacement.metadata.uid = "other";
    const editor = conditionalEditor(
      store,
      "u",
      "/save",
      async (_url, init) => {
        if (init?.method === "PATCH") return new Response(null, { status: 409 });
        return Response.json({ object: replacement, redactedPaths: [] }, { status });
      },
      () => true,
    );
    if (status === 403) await assert.rejects(editor.save(), /HTTP 403/);
    else assert.equal(await editor.save(), "unavailable");
    assert.equal(editor.saving, false);
    assert.equal(store.server("u").metadata.resourceVersion, "1");
  }
});

test("editor serializes requests and releases busy state after transport failure", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  store.setValue("u", ["data", "value"], "mine");
  let reject!: (error: Error) => void;
  const pending = new Promise<Response>((_resolve, fail) => {
    reject = fail;
  });
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    () => pending,
    () => true,
  );
  const saving = editor.save();
  assert.equal(await editor.save(), "busy");
  reject(new Error("offline"));
  await assert.rejects(saving, /offline/);
  assert.equal(editor.saving, false);
});

test("unknown redaction metadata blocks writes until an authoritative update enables reconciliation", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  store.setValue("u", ["data", "value"], "mine");
  let patches = 0;
  const editor = conditionalEditor(
    store,
    "u",
    "/save",
    async (_url, init) => {
      if (init?.method === "PATCH") {
        patches++;
        return new Response(null, { status: 409 });
      }
      return Response.json({ object: object("2"), redactedPaths: ["/data/token"] });
    },
    () => true,
  );
  assert.equal(await editor.save(), "recovering");
  assert.equal(await editor.save(), "recovering");
  assert.equal(patches, 1);
  store.applyServerEvent(object("2"), { redacted: [{ path: "/data/token", rev: 7 }] });
  assert.equal(await editor.save(), "version-stale");
  assert.equal(patches, 1);
  assert.deepEqual(store.redactions("u"), [{ path: ["data", "token"], rev: 7 }]);
});

test("editor forwards both redaction formats and preserves omitted metadata", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  for (const metadata of [
    { redacted: [{ path: "/data/token", rev: 9 }] },
    {},
    { redactedPaths: ["/data/token"], redacted: [{ path: "/data/token", rev: 99 }] },
  ]) {
    const store = new LiveResourceStore();
    store.applyServerEvent(object("1"), { redacted: [{ path: "/data/token", rev: 7 }] });
    store.setValue("u", ["metadata", "labels", "edited"], "yes");
    const editor = conditionalEditor(
      store,
      "u",
      "/save",
      async (_url, init) => {
        if (init?.method === "PATCH") return new Response(null, { status: 409 });
        return Response.json({ object: object("2"), ...metadata });
      },
      () => true,
    );
    assert.equal(await editor.save(), "version-stale");
    assert.deepEqual(store.redactions("u"), [
      {
        path: ["data", "token"],
        rev: "redactedPaths" in metadata ? 7 : (metadata.redacted?.[0]?.rev ?? 7),
      },
    ]);
    assert.equal(store.isEditable("u", ["data", "token"]), false);
  }
});
