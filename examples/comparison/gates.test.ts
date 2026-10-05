// The measurement's gates, without a host or a cluster. Run by `task test-compare`.

import assert from "node:assert/strict";
import { test } from "node:test";
import { LiveResourceStore } from "../../packages/krm-stream/src/index.ts";
import { divergence, type StateObject, workloadFailure } from "./gates.ts";

test("a workload that did not do everything it planned is rejected", () => {
  const planned = { spec: 2, secret: 1, disconnect: 2 };
  assert.equal(workloadFailure({ planned, done: { ...planned } }), undefined);
  // Every PATCH failed: the stores still match the unchanged cluster, so only this gate catches it.
  assert.match(
    workloadFailure({ planned, done: { disconnect: 2 }, errors: ["patch w-00: forbidden", "patch s-0: forbidden"] }) ??
      "",
    /2 workload step\(s\) failed: patch w-00: forbidden \(and 1 more\)/,
  );
  // A step that neither succeeded nor reported an error still leaves the run incomplete.
  assert.match(workloadFailure({ planned, done: { spec: 2, disconnect: 2 } }) ?? "", /0 of 1 planned secret/);
  assert.match(workloadFailure(undefined) ?? "", /no workload outcome/);
});

const secret = (rv: string): StateObject => ({
  kind: "Secret",
  uid: "uid-s0",
  name: "s-0",
  resourceVersion: rv,
  data: { token: btoa(`token@${rv}`) },
});
/** A projected Secret as full and spec deliver it: no data, the key as a redacted path. */
const projected = (rv: string) => ({
  apiVersion: "v1",
  kind: "Secret",
  metadata: { uid: "uid-s0", name: "s-0", resourceVersion: rv },
  type: "Opaque",
});

test("a projected Secret store must hold the final successful rotation, which may coalesce", () => {
  const store = new LiveResourceStore();
  store.applyServerEvent(projected("10"), { redacted: [{ path: "/data/token", rev: 1 }] });
  const feed = { source: "spec" as const, resource: "secrets" as const, store };
  const cluster = [secret("30")];

  // Without the rotation evidence the stale store passes: same key, value withheld.
  assert.equal(divergence(feed, cluster), undefined);
  // With it, the store that missed the rotation is caught.
  assert.match(divergence(feed, cluster, { "s-0": "30" }) ?? "", /holds version 10, not its final rotation 30/);

  // The rotation at 20 coalesced away: the store receives only the final one, at 30.
  store.applyServerEvent(projected("30"), { redacted: [{ path: "/data/token", rev: 2 }] });
  assert.equal(divergence(feed, cluster, { "s-0": "30" }), undefined);
  // A Secret the workload never rotated has no requirement beyond its paths.
  assert.equal(divergence(feed, cluster, {}), undefined);
});

test("a native Secret store is still held to the cluster's exact value and version", () => {
  const store = new LiveResourceStore();
  const object = secret("30");
  store.applyServerEvent({
    apiVersion: "v1",
    kind: "Secret",
    metadata: { uid: object.uid, name: object.name, resourceVersion: "20" },
    data: { token: btoa("token@20") },
  });
  const feed = { source: "native" as const, resource: "secrets" as const, store };
  assert.match(divergence(feed, [object], { "s-0": "30" }) ?? "", /data differs/);
});
