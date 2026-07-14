// The invariants of docs/client-state-model.md §7 that no fixture pins.
//
// The conformance corpus is the contract, and it is where the rules that BOTH implementations owe
// each other live. These are the ones only the client can break — they need no gateway, no wire, and
// nothing to agree with. They belong here, not in conformance/.
//
// The one worth reading twice is I-READONLY. status-follow-live proves `status` follows the server
// and never becomes dirty, but the user in that fixture never TRIES to edit it — so the fixture
// stays green even against a store that would happily let them. "Structurally incapable of becoming
// an edit" is a claim about what happens when someone tries, and something has to try.

import assert from "node:assert/strict";
import { test } from "node:test";
import { defaultPolicy, LiveResourceStore, readOnlyPolicy, withOpenAPIKeyedLists } from "../src/index.ts";
import type { KRMObject, Path } from "../src/types.ts";
import { body } from "./conformance.ts";

const CM = "cm-app.v1"; // ConfigMap: data.log-level=info, data.replicas="3", one dotted label
const DEPLOY = "deploy-web.v1"; // Deployment: nested spec, a container array, a live status

function seeded(ref: string, redacted: { path: string; rev: number }[] = []): [LiveResourceStore, string] {
  const store = new LiveResourceStore();
  const obj = body(ref);
  store.beginSnapshot();
  store.applyServerEvent(obj, { redacted });
  store.endSnapshot();
  return [store, obj.metadata.uid];
}

test("I-READONLY — an edit to status is refused, not merely ignored", () => {
  const [store, id] = seeded(DEPLOY);
  const paths: Path[] = [
    ["status", "readyReplicas"],
    ["status", "conditions"],
    ["metadata", "name"],
    ["metadata", "resourceVersion"],
    ["apiVersion"],
  ];
  for (const p of paths) {
    assert.equal(store.isEditable(id, p), false, `${JSON.stringify(p)} must not be editable`);
    assert.throws(() => store.setValue(id, p, 99), /read-only/, `setValue(${JSON.stringify(p)})`);
    assert.throws(() => store.removeKey(id, p), /read-only/, `removeKey(${JSON.stringify(p)})`);
    assert.equal(store.isDirty(id, p), false);
  }
  assert.equal(store.patch(id), null);
});

test("I-READONLY — the status-watch policy makes the whole object unsaveable", () => {
  const store = new LiveResourceStore(readOnlyPolicy);
  const obj = body(DEPLOY);
  store.applyServerEvent(obj);
  const id = obj.metadata.uid;

  assert.throws(() => store.setValue(id, ["spec", "replicas"], 5), /read-only/);
  // ...but it still FOLLOWS the server and still flashes. Read-only is not ignored — it is the
  // entire live-status-watch use case, and a viewer that stopped updating would be worthless.
  const r = store.applyServerEvent(body("deploy-web.v2"));
  assert.deepEqual((store.status(id) as { readyReplicas: number }).readyReplicas, 2);
  assert.ok(r.flashed.some((p) => JSON.stringify(p) === JSON.stringify(["status", "readyReplicas"])));
  assert.equal(store.patch(id), null);
});

test("I-ORDER-EQ — a reordered-but-equal object is not a change, and must not flash", () => {
  const [store, id] = seeded(DEPLOY);
  const reordered = reorder(body(DEPLOY));
  assert.notEqual(JSON.stringify(reordered), JSON.stringify(body(DEPLOY)), "the test object must really be reordered");

  const r = store.applyServerEvent(reordered);
  assert.deepEqual(r.flashed, [], "a key-order change is not a change — JSON.stringify equality says it is");
  assert.equal(r.structural, false);
  assert.equal(store.patch(id), null);
});

test("I-IDEMPOTENT — redelivering the same object is a no-op (a reconnect replays the snapshot)", () => {
  const [store, id] = seeded(CM);
  store.setValue(id, ["data", "log-level"], "debug");

  const before = store.draft(id);
  const r = store.applyServerEvent(body(CM));
  assert.deepEqual(store.draft(id), before, "the redelivery moved the draft");
  assert.deepEqual(r.flashed, []);
  assert.deepEqual(store.conflicts(id), [], "the object did not change — there is nothing to conflict with");
  assert.deepEqual(store.patch(id), { data: { "log-level": "debug" } }, "and the edit is still pending");
});

test("I-BASESHIFT — the second event reconciles against the first, not against the snapshot", () => {
  const [store, id] = seeded(CM);
  store.applyServerEvent(body("cm-app.v3")); // log-level: info -> warn. Nobody is editing it.
  assert.equal((store.draft(id).data as Record<string, string>)["log-level"], "warn");

  // Now the user edits it, and the server sends v3 AGAIN. Against the ORIGINAL snapshot as base this
  // would look like "the server just changed log-level" and false-conflict; against v3 it is a no-op.
  store.setValue(id, ["data", "log-level"], "debug");
  store.applyServerEvent(body("cm-app.v3"));
  assert.deepEqual(store.conflicts(id), []);
  assert.deepEqual(store.patch(id), { data: { "log-level": "debug" } });
});

test("I-KEYSAFE — dots, slashes and a key that merely LOOKS like an array index", () => {
  const [store, id] = seeded(CM);
  store.setValue(id, ["metadata", "labels", "app.kubernetes.io/name"], "checkout");
  store.addKey(id, ["data"], "0", "a key literally named zero");
  store.addKey(id, ["metadata", "annotations"], "a~b/c.d", "tilde and slash and dot");

  assert.deepEqual(store.patch(id), {
    metadata: {
      labels: { "app.kubernetes.io/name": "checkout" },
      annotations: { "a~b/c.d": "tilde and slash and dot" },
    },
    data: { "0": "a key literally named zero" },
  });
  assert.ok(store.isDirty(id, ["data", "0"]));
  assert.ok(store.isDirty(id, ["metadata", "labels", "app.kubernetes.io/name"]));
});

test("removeKey and renameKey — a deletion is a null in the patch, and a rename is both", () => {
  const [store, id] = seeded(CM);
  store.removeKey(id, ["data", "replicas"]);
  assert.deepEqual(store.patch(id), { data: { replicas: null } });

  const [store2, id2] = seeded(CM);
  store2.renameKey(id2, ["data"], "log-level", "logLevel");
  assert.deepEqual(store2.patch(id2), { data: { "log-level": null, logLevel: "info" } });
  assert.deepEqual(Object.keys(store2.draft(id2).data as object), ["logLevel", "replicas"], "order is preserved");
});

test("renameKey refuses to overwrite an existing key", () => {
  const [store, id] = seeded(CM);
  assert.throws(() => store.renameKey(id, ["data"], "log-level", "replicas"), /already exists/);
  assert.deepEqual(store.patch(id), null);
});

test("I-PATCH-ROUNDTRIP — applying patch(id) to the server object yields the draft", () => {
  const [store, id] = seeded(DEPLOY);
  store.setValue(id, ["spec", "replicas"], 5);
  store.setValue(id, ["spec", "template", "spec", "containers", 0, "image"], "ghcr.io/x/web:v2");
  store.removeKey(id, ["spec", "paused"]);
  store.setValue(id, ["metadata", "labels", "app.kubernetes.io/name"], "checkout");

  const patched = applyMergePatch(store.server(id), store.patch(id));
  assert.deepEqual(patched, store.draft(id));
  // Arrays go whole (§4.1, and RFC 7386 agrees) — never as an index-addressed sub-patch.
  const spec = (store.patch(id) as { spec: { template: { spec: { containers: unknown[] } } } }).spec;
  assert.ok(Array.isArray(spec.template.spec.containers));
  assert.equal(spec.template.spec.containers.length, 1);
});

test("OpenAPI associative lists merge a keyed element across server reordering", () => {
  const schema = {
    properties: {
      spec: {
        properties: {
          template: {
            properties: {
              spec: {
                properties: {
                  containers: {
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-list-map-keys": ["name"],
                    items: { properties: { name: {}, image: {}, resources: {} } },
                  },
                },
              },
            },
          },
        },
      },
    },
  };
  const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, schema));
  const base = deploymentWithContainers([
    { name: "web", image: "example/web:v1", resources: { requests: { cpu: "100m" } } },
  ]);
  store.applyServerEvent(base);
  const id = base.metadata.uid;

  // The edit was made when `web` was index zero. The server later prepends another container and
  // changes a different field on web, so a positional merge would conflict or edit the wrong item.
  store.setValue(id, ["spec", "template", "spec", "containers", 0, "image"], "example/web:v2");
  const incoming = deploymentWithContainers([
    { name: "sidecar", image: "example/sidecar:v1" },
    { name: "web", image: "example/web:v1", resources: { requests: { cpu: "250m" } } },
  ]);
  const result = store.applyServerEvent(incoming);
  const containers = containersOf(store.draft(id));

  assert.deepEqual(containers, [
    { name: "sidecar", image: "example/sidecar:v1" },
    { name: "web", image: "example/web:v2", resources: { requests: { cpu: "250m" } } },
  ]);
  assert.deepEqual(result.conflicts, [], "independent keyed-element changes must not conflict");
  assert.deepEqual(containersOf(store.patch(id) as KRMObject), containers, "RFC 7386 still sends one complete array");
});

test("OpenAPI associative lists keep a same-field conflict on the keyed element", () => {
  const schema = {
    properties: {
      spec: {
        properties: {
          template: {
            properties: {
              spec: {
                properties: {
                  containers: {
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-list-map-keys": ["name"],
                    items: { properties: { name: {}, image: {} } },
                  },
                },
              },
            },
          },
        },
      },
    },
  };
  const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, schema));
  const base = deploymentWithContainers([{ name: "web", image: "example/web:v1" }]);
  store.applyServerEvent(base);
  const id = base.metadata.uid;
  store.setValue(id, ["spec", "template", "spec", "containers", 0, "image"], "example/web:ours");
  store.applyServerEvent(deploymentWithContainers([{ name: "web", image: "example/web:theirs" }]));

  assert.deepEqual(store.conflicts(id), [
    {
      path: ["spec", "template", "spec", "containers", 0, "image"],
      theirs: "example/web:theirs",
    },
  ]);
});

test("OpenAPI associative-list conflicts follow their entry across a reorder", () => {
  const schema = {
    properties: {
      spec: {
        properties: {
          template: {
            properties: {
              spec: {
                properties: {
                  containers: {
                    "x-kubernetes-list-type": "map",
                    "x-kubernetes-list-map-keys": ["name"],
                    items: { properties: { name: {}, image: {} } },
                  },
                },
              },
            },
          },
        },
      },
    },
  };
  const store = new LiveResourceStore(withOpenAPIKeyedLists(defaultPolicy, schema));
  const base = deploymentWithContainers([{ name: "web", image: "example/web:v1" }]);
  store.applyServerEvent(base);
  const id = base.metadata.uid;
  store.setValue(id, ["spec", "template", "spec", "containers", 0, "image"], "example/web:ours");
  store.applyServerEvent(deploymentWithContainers([{ name: "web", image: "example/web:theirs" }]));

  // The conflict already exists. A later prepend must move its public, index-shaped path to web's
  // new location rather than leaving the UI pointing at sidecar.
  store.applyServerEvent(
    deploymentWithContainers([
      { name: "sidecar", image: "example/sidecar:v1" },
      { name: "web", image: "example/web:theirs" },
    ]),
  );
  assert.deepEqual(store.conflicts(id), [
    {
      path: ["spec", "template", "spec", "containers", 1, "image"],
      theirs: "example/web:theirs",
    },
  ]);
});

test("revert — takes the server's value back, and resolves the conflict with it", () => {
  const [store, id] = seeded(CM);
  store.setValue(id, ["data", "log-level"], "debug");
  store.applyServerEvent(body("cm-app.v3")); // the server says warn: a real conflict
  assert.deepEqual(store.conflicts(id), [{ path: ["data", "log-level"], theirs: "warn" }]);

  store.revert(id, ["data", "log-level"]);
  assert.deepEqual(store.conflicts(id), [], "taking theirs resolves the conflict");
  assert.equal(store.isDirty(id, ["data", "log-level"]), false);
  assert.equal(store.patch(id), null);
});

test("adoptSaved — the save landed; stop showing it as dirty without waiting for the echo", () => {
  const [store, id] = seeded(CM);
  store.setValue(id, ["data", "log-level"], "debug");
  store.adoptSaved(body("cm-app.v4")); // what the API server returned
  assert.equal(store.patch(id), null);
  assert.deepEqual(store.conflicts(id), []);

  const r = store.applyServerEvent(body("cm-app.v4")); // ...and the watch echoes it: a no-op
  assert.deepEqual(r.flashed, []);
  assert.equal(store.patch(id), null);
});

test("adoptSaved preserves an edit made after the save request", () => {
  const [store, id] = seeded(CM);
  store.setValue(id, ["data", "log-level"], "debug");
  store.setValue(id, ["data", "replicas"], "4"); // made after the first edit was submitted
  store.adoptSaved(body("cm-app.v4")); // the server accepted log-level=debug, but not the later edit

  assert.equal((store.draft(id).data as Record<string, string>)["log-level"], "debug");
  assert.deepEqual(store.patch(id), { data: { replicas: "4" } });
});

test("I-REDACT — a redacted value is ABSENT, is read-only, and cannot reach a patch", () => {
  const [store, id] = seeded("secret-token.v1-wire", [
    { path: "/data/token", rev: 1 },
    { path: "/data/username", rev: 1 },
  ]);
  assert.equal(store.isEditable(id, ["data", "token"]), false);
  assert.throws(() => store.setValue(id, ["data", "token"], "hunter2"), /read-only/);
  assert.throws(() => store.removeKey(id, ["data", "token"]), /read-only/);
  assert.equal(store.isEditable(id, ["data"]), false, "an ancestor could overwrite withheld values");
  assert.throws(() => store.setValue(id, ["data"], { token: "hunter2" }), /read-only/);

  // The value is GONE — not masked (proposal 0003), and `data` does not survive as an empty map
  // either: a map that is empty only because the gateway emptied it is the gateway's artifact, not
  // the server's state. There is no placeholder to hold, and therefore none to save back over the
  // real secret. The hazard cannot arise rather than being guarded.
  assert.equal(store.draft(id).data, undefined, "`data` survived the projection");

  // …and keys-only disclosure survives, because `redacted` carries it: the consumer still knows
  // `token` exists, which is what a UI renders `token ••••••` from.
  assert.deepEqual(store.redactions(id), [
    { path: ["data", "token"], rev: 1 },
    { path: ["data", "username"], rev: 1 },
  ]);

  store.setValue(id, ["metadata", "labels", "app.kubernetes.io/name"], "checkout");
  assert.deepEqual(store.patch(id), { metadata: { labels: { "app.kubernetes.io/name": "checkout" } } });
});

test("unknown resource — every query says so rather than inventing an empty object", () => {
  const store = new LiveResourceStore();
  assert.deepEqual(store.ids(), []);
  assert.throws(() => store.draft("nope"), /no resource/);
  assert.throws(() => store.patch("nope"), /no resource/);
});

/** A structurally identical object with its keys emitted in a different order — what a re-serialized
 * object from an API server legitimately looks like. */
function reorder<T>(v: T): T {
  if (Array.isArray(v)) return v.map(reorder) as unknown as T;
  if (v && typeof v === "object") {
    const entries = Object.entries(v as Record<string, unknown>).reverse();
    return Object.fromEntries(entries.map(([k, x]) => [k, reorder(x)])) as T;
  }
  return v;
}

/** RFC 7386, the ~10 lines of it, so the roundtrip is checked against the SPEC and not against the
 * same code that produced the patch. */
function applyMergePatch(target: unknown, patch: unknown): unknown {
  if (!patch || typeof patch !== "object" || Array.isArray(patch)) return patch;
  const out: Record<string, unknown> =
    target && typeof target === "object" && !Array.isArray(target) ? { ...(target as Record<string, unknown>) } : {};
  for (const [k, v] of Object.entries(patch as Record<string, unknown>)) {
    if (v === null) delete out[k];
    else out[k] = applyMergePatch(out[k], v);
  }
  return out;
}

function deploymentWithContainers(containers: Array<Record<string, unknown>>) {
  return {
    apiVersion: "apps/v1",
    kind: "Deployment",
    metadata: { uid: "keyed-deploy", name: "web", resourceVersion: "1" },
    spec: { template: { spec: { containers } } },
  };
}

function containersOf(object: KRMObject): Array<Record<string, unknown>> {
  return (object.spec as { template: { spec: { containers: Array<Record<string, unknown>> } } }).template.spec
    .containers;
}
