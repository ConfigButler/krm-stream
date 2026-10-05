// The editor recipes in examples/editor-recipes, executed. They are copyable host code rather than
// library API, so these tests are what makes the guidance that links to them true.
//
// Every server change arrives as a stream event, the way connectResourceStream delivers it.

import assert from "node:assert/strict";
import { test } from "node:test";
import { keepLocal } from "../../../examples/editor-recipes/keepLocal.ts";
import { retainRecoveryCopy } from "../../../examples/editor-recipes/recoveryCopy.ts";
import { applyStreamEvent, type KRMObject, LiveResourceStore } from "../src/index.ts";

const widget = (uid: string, rv: string, spec: Record<string, unknown>): KRMObject => ({
  apiVersion: "example.com/v1",
  kind: "Widget",
  metadata: { uid, name: "w", namespace: "app", resourceVersion: rv },
  spec,
  status: { ready: true },
});
const identity = (uid: string) => ({ uid, apiVersion: "example.com/v1", kind: "Widget", name: "w", namespace: "app" });

function storeWith(...objects: KRMObject[]) {
  const store = new LiveResourceStore();
  applyStreamEvent(store, { type: "reset" });
  for (const object of objects) applyStreamEvent(store, { type: "added", object });
  applyStreamEvent(store, { type: "synced" });
  return store;
}

// ------------------------------------------------------------------------- deletion recovery --

test("recovery: the copy taken when the recipe starts survives an immediate deletion", () => {
  const store = storeWith(widget("u", "1", { replicas: 1 }));
  store.setValue("u", ["spec", "replicas"], 3); // typed before the recipe started
  const recovery = retainRecoveryCopy(store, "u", { now: () => 1000 });
  assert.equal(recovery.copy(), null, "no copy while the object exists: edit the live draft");
  assert.equal(recovery.removed, false);

  applyStreamEvent(store, { type: "deleted", identity: identity("u") });
  const copy = recovery.copy();
  assert.equal(recovery.removed, true);
  assert.deepEqual(copy?.draft.spec, { replicas: 3 });
  assert.deepEqual(
    copy?.edits.map((e) => [e.path, e.new]),
    [[["spec", "replicas"], 3]],
  );
  assert.deepEqual(copy?.identity, { apiVersion: "example.com/v1", kind: "Widget", name: "w", namespace: "app" });
  assert.equal(copy?.removedAt, 1000);
});

test("recovery: a host listener subscribed before the recipe already sees the removal", () => {
  const store = storeWith(widget("u", "1", { replicas: 1 }));
  const seen: unknown[] = [];
  // A page renders from its own listener, which it subscribed first, and reads the copy there.
  store.subscribe(() => seen.push([recovery.removed, recovery.copy()?.draft.spec ?? null]));
  const recovery = retainRecoveryCopy(store, "u");
  store.setValue("u", ["spec", "replicas"], 3);
  applyStreamEvent(store, { type: "deleted", identity: identity("u") });
  assert.deepEqual(seen.at(-1), [true, { replicas: 3 }]);
});

test("recovery: typing just before snapshot pruning is kept", () => {
  const store = storeWith(widget("u", "1", { replicas: 1, image: "v1" }));
  const recovery = retainRecoveryCopy(store, "u");
  store.setValue("u", ["spec", "image"], "v2");
  applyStreamEvent(store, { type: "reset" });
  store.setValue("u", ["spec", "replicas"], 5); // the last keystroke, mid-resync
  applyStreamEvent(store, { type: "synced" }); // deleted while disconnected: pruned here
  assert.deepEqual(store.ids(), []);
  assert.deepEqual(recovery.copy()?.draft.spec, { replicas: 5, image: "v2" });
});

test("recovery: a replacement under the same name never inherits the old draft", () => {
  const store = storeWith(widget("old", "1", { replicas: 1 }));
  const recovery = retainRecoveryCopy(store, "old");
  store.setValue("old", ["spec", "replicas"], 9);
  applyStreamEvent(store, { type: "deleted", identity: identity("old") });
  applyStreamEvent(store, { type: "added", object: widget("new", "2", { replicas: 1 }) });

  assert.deepEqual(store.draft("new").spec, { replicas: 1 }, "the replacement opens clean");
  assert.deepEqual(store.changes("new"), []);
  const copy = recovery.copy();
  assert.equal(copy?.uid, "old", "the copy stays bound to the identity it was made against");
  assert.deepEqual(copy?.draft.spec, { replicas: 9 });
});

test("recovery: an object never in the store has nothing to recover", () => {
  const store = storeWith();
  const recovery = retainRecoveryCopy(store, "missing");
  applyStreamEvent(store, { type: "reset" });
  applyStreamEvent(store, { type: "synced" });
  assert.equal(recovery.removed, false);
  assert.equal(recovery.copy(), null);
});

test("recovery: copies are detached, expire, and dispose releases the subscription", () => {
  let clock = 0;
  const store = storeWith(widget("u", "1", { replicas: 1 }), widget("v", "1", { replicas: 1 }));
  const recovery = retainRecoveryCopy(store, "u", { retainMs: 100, now: () => clock });
  store.setValue("u", ["spec", "replicas"], 2);
  applyStreamEvent(store, { type: "deleted", identity: identity("u") });

  const first = recovery.copy()!;
  (first.draft.spec as Record<string, unknown>).replicas = 99;
  assert.deepEqual(recovery.copy()?.draft.spec, { replicas: 2 }, "a caller cannot change the retained copy");
  clock = 100;
  assert.notEqual(recovery.copy(), null, "still within its lifetime");
  clock = 101;
  assert.equal(recovery.copy(), null, "expired");

  const other = retainRecoveryCopy(store, "v");
  other.dispose();
  applyStreamEvent(store, { type: "deleted", identity: identity("v") });
  assert.equal(other.removed, false, "a disposed recipe no longer listens");
  assert.equal(other.copy(), null);
});

// --------------------------------------------------------------------------- keep-local --

/** A store holding one object whose draft conflicts with a server change. */
function conflicted(
  base: Record<string, unknown>,
  edit: (store: LiveResourceStore) => void,
  theirs: Record<string, unknown>,
) {
  const store = storeWith(widget("u", "1", base));
  edit(store);
  applyStreamEvent(store, { type: "modified", object: widget("u", "2", theirs) });
  return store;
}
const conflictPaths = (store: LiveResourceStore) => store.conflicts("u").map((c) => c.path.join("."));

test("keep-local: a nested value is kept, other edits and conflicts are not touched", () => {
  const store = conflicted(
    { a: { x: "base" }, b: "base", c: "base" },
    (s) => {
      s.setValue("u", ["spec", "a", "x"], "mine");
      s.setValue("u", ["spec", "b"], "mine");
      s.setValue("u", ["spec", "c"], "unrelated edit");
    },
    { a: { x: "theirs" }, b: "theirs", c: "base" },
  );
  assert.deepEqual(conflictPaths(store), ["spec.a.x", "spec.b"]);

  assert.equal(keepLocal(store, "u", ["spec", "a", "x"]), true);
  assert.deepEqual(conflictPaths(store), ["spec.b"], "the other conflict waits for its own decision");
  assert.deepEqual(store.draft("u").spec, { a: { x: "mine" }, b: "mine", c: "unrelated edit" });

  // A fresh intent after review: the kept value, bound to the version the person reviewed.
  const intent = store.captureSave("u");
  assert.equal(intent?.resourceVersion, "2");
  assert.deepEqual(intent?.patch, { spec: { a: { x: "mine" }, b: "mine", c: "unrelated edit" } });
});

test("keep-local: a local deletion stays deleted, and a value the server deleted comes back", () => {
  const store = conflicted(
    { gone: "base", kept: "base" },
    (s) => {
      s.removeKey("u", ["spec", "gone"]); // the person deleted it…
      s.setValue("u", ["spec", "kept"], "mine"); // …and changed this one
    },
    { gone: "theirs" }, // the server changed the first and deleted the second
  );
  assert.deepEqual(conflictPaths(store), ["spec.gone", "spec.kept"]);
  assert.equal(keepLocal(store, "u", ["spec", "gone"]), true);
  assert.equal(keepLocal(store, "u", ["spec", "kept"]), true);
  assert.deepEqual(store.conflicts("u"), []);
  assert.deepEqual(store.draft("u").spec, { kept: "mine" });
  assert.deepEqual(store.captureSave("u")?.patch, { spec: { gone: null, kept: "mine" } });
});

test("keep-local: arrays are kept whole, and a parent decision resolves nested conflicts", () => {
  const store = conflicted(
    { list: ["a", "b"], group: { x: "base", y: "base" } },
    (s) => {
      s.setValue("u", ["spec", "list"], ["a", "b", "mine"]);
      s.setValue("u", ["spec", "group", "x"], "mine");
      s.setValue("u", ["spec", "group", "y"], "mine");
    },
    { list: ["theirs"], group: { x: "theirs", y: "theirs" } },
  );
  assert.deepEqual(conflictPaths(store), ["spec.list", "spec.group.x", "spec.group.y"]);
  assert.equal(keepLocal(store, "u", ["spec", "list"]), true);
  assert.equal(keepLocal(store, "u", ["spec", "group"]), true);
  assert.deepEqual(store.conflicts("u"), []);
  assert.deepEqual(store.captureSave("u")?.patch, {
    spec: { list: ["a", "b", "mine"], group: { x: "mine", y: "mine" } },
  });
});

test("keep-local: a read-only or redacted path is refused and nothing changes", () => {
  const store = storeWith(widget("u", "1", { a: "base" }));
  const before = store.draft("u");
  assert.equal(keepLocal(store, "u", ["status", "ready"]), false, "status is controller-owned");
  assert.deepEqual(store.draft("u"), before);

  // A redaction can appear after the conflict did: the value is then not the person's to keep.
  const secret = (rv: string, data: Record<string, unknown>, redacted: { path: string; rev: number }[] = []) => ({
    type: "modified" as const,
    object: { apiVersion: "v1", kind: "Secret", metadata: { uid: "s", name: "s", resourceVersion: rv }, data },
    redacted,
  });
  applyStreamEvent(store, { ...secret("1", { token: "base", user: "base" }), type: "added" });
  store.setValue("s", ["data", "token"], "mine");
  store.setValue("s", ["data", "user"], "mine");
  applyStreamEvent(store, secret("2", { token: "theirs", user: "theirs" }));
  assert.deepEqual(
    store.conflicts("s").map((c) => c.path.join(".")),
    ["data.token", "data.user"],
  );
  applyStreamEvent(store, secret("3", { user: "theirs" }, [{ path: "/data/token", rev: 1 }]));
  const draft = store.draft("s");
  const conflicts = store.conflicts("s");
  assert.equal(keepLocal(store, "s", ["data", "token"]), false);
  assert.deepEqual(store.draft("s"), draft);
  assert.deepEqual(store.conflicts("s"), conflicts);
  assert.equal(keepLocal(store, "s", ["data", "user"]), true, "the unredacted value can still be kept");
});
