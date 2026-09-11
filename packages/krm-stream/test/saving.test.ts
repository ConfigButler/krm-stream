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
  const editor = conditionalEditor(store, "u", "/save", async (_url, init) => {
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
  });
  assert.equal(await editor.save(), "conflict");
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
  const editor = conditionalEditor(store, "u", "/save", async (_url, init) => {
    if (init?.method === "PATCH") return new Response(null, { status: 409 });
    store.applyServerEvent(object("3", "watch"));
    return Response.json({ object: object("2", "stale GET"), redactedPaths: [] });
  });
  assert.equal(await editor.save(), "conflict");
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

test("save after deletion returns conflict without making a request", async () => {
  const { conditionalEditor } = await import("../../../examples/conditional-save/editor.ts");
  const store = new LiveResourceStore();
  store.applyServerEvent(object("1"));
  const editor = conditionalEditor(store, "u", "/save", async () => {
    assert.fail("request after deletion");
  });
  store.removeResource("u");
  assert.equal(await editor.save(), "conflict");
});
