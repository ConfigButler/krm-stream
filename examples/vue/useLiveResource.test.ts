import assert from "node:assert/strict";
import { test } from "node:test";
import { effectScope } from "vue";
import { LiveResourceStore, connectManagedResourceStream } from "../../packages/krm-stream/src/index.ts";
import { useLiveResource } from "./useLiveResource.ts";

test("Vue refs follow drafts, conflicts, redactions, deletion and connection state", async () => {
  const store = new LiveResourceStore();
  const object = {
    apiVersion: "v1",
    kind: "Secret",
    metadata: { uid: "u", name: "secret", resourceVersion: "1", labels: { value: "base" } },
    data: {},
  };
  store.applyServerEvent(object, { redacted: [{ path: "/data/token", rev: 3 }] });
  const connection = connectManagedResourceStream("/stream", store, {
    maxRetries: 0,
    fetch: async () => {
      throw new Error("offline");
    },
  });
  const scope = effectScope();
  const view = scope.run(() => useLiveResource(store, "u", connection))!;
  store.setValue("u", ["metadata", "labels", "value"], "mine");
  assert.deepEqual(view.resource.value?.draft.metadata.labels, { value: "mine" });
  assert.equal(view.resource.value?.changes.length, 1);
  store.applyServerEvent(
    { ...object, metadata: { ...object.metadata, labels: { value: "theirs" } } },
    { redacted: [{ path: "/data/token", rev: 4 }] },
  );
  assert.equal(view.resource.value?.conflicts.length, 1);
  assert.equal(view.resource.value?.redactions[0]?.rev, 4);
  await connection.closed;
  assert.equal(view.state.value.status, "exhausted");
  store.removeResource("u");
  assert.equal(view.resource.value, null);
  scope.stop();
});

test("disposing an editor stops both subscriptions and leaves shared connections owned by the host", async () => {
  const store = new LiveResourceStore();
  const connection = connectManagedResourceStream("/stream", store, {
    maxRetries: 0,
    fetch: async () => new Response(new ReadableStream()),
  });
  const scope = effectScope();
  const view = scope.run(() => useLiveResource(store, "u", connection))!;
  scope.stop();
  store.applyServerEvent({ apiVersion: "v1", kind: "ConfigMap", metadata: { uid: "u", name: "cm" } });
  assert.equal(view.resource.value, null);
  await new Promise<void>((resolve) => {
    const stop = connection.subscribe((state) => {
      if (state.status === "syncing") {
        stop();
        resolve();
      }
    });
  });
  assert.equal(connection.state.status, "syncing");
  assert.equal(view.state.value.status, "connecting");
  connection.close();
  await connection.closed;
  assert.equal(view.state.value.status, "connecting");
});
