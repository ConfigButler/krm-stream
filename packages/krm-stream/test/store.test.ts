// The client's half of the conformance corpus, fed as OBJECTS.
//
// Every client fixture is replayed event by event into a LiveResourceStore, the fixture's edits are
// applied at their `after:` positions, and what the store then holds is compared to `expect:`.
//
// The `after:` ordering is the whole point. An edit placed BEFORE the server change that collides
// with it is the only way to test a three-way merge at all — a suite that edited only at the end
// would pass against a store that just follows the server.
//
// Its sibling, wire.test.ts, runs the SAME assertions against the same fixtures delivered as the
// BYTES the Go gateway actually wrote. Together they are the seam: this file proves the store is
// right, that one proves the store is right about what the gateway really said.

import assert from "node:assert/strict";
import { test } from "node:test";
import { applyStreamEvent, LiveResourceStore } from "../src/index.ts";
import type { Path } from "../src/types.ts";
import { clientFixtures, resolve } from "./conformance.ts";
import { applyEdit, check } from "./expect.ts";

for (const f of clientFixtures()) {
  test(`${f.id}: ${f.title}`, () => {
    const store = new LiveResourceStore();
    const flashed: Path[] = [];

    for (const [i, fe] of f.events.entries()) {
      // applyStreamEvent is library code, not test code: the switch from event to store call IS the
      // protocol (spec §4), and a host feeding a store from its own transport must not have to
      // reimplement it and get `synced` subtly wrong.
      flashed.push(...applyStreamEvent(store, resolve(f, fe)).flashed);

      for (const edit of f.client?.edits ?? []) {
        if (edit.after === i) applyEdit(store, edit);
      }
      for (const cp of f.checkpoints ?? []) {
        if (cp.after === i) check(store, flashed, cp, `${f.id} @checkpoint after=${i}`);
      }
    }

    if (f.client?.expect) check(store, flashed, f.client.expect, f.id);
  });
}

// A suite that silently runs zero fixtures is the failure mode this whole repo exists to prevent.
test("the client suite actually ran the corpus", () => {
  assert.ok(clientFixtures().length >= 10, "the client half of the corpus is missing fixtures");
});

// The gap that sent the first host to adopt this back to writing its own EventSource loop.
//
// applyStreamEvent used to return Path[] — the flashed paths and nothing else — which is unusable the
// moment a stream carries more than one resource: a UI learns that something moved, and not what. So
// a host that wanted to highlight per-resource had to abandon connectWithEventSource, drive its own
// EventSource, and reimplement the event switch. That is precisely the work this library exists to do
// once, correctly, on everyone's behalf.
//
// It returns the whole StreamChange now. This pins each field, because each one was being computed in
// the store and thrown away at the seam.
test("a change says WHICH resource changed, and what kind of change it was", () => {
  const store = new LiveResourceStore();
  const object = {
    apiVersion: "v1",
    kind: "ConfigMap",
    metadata: { uid: "u-1", name: "app", namespace: "default", resourceVersion: "1" },
    data: { greeting: "hello" },
  };

  store.beginSnapshot();
  const arrival = applyStreamEvent(store, { seq: 1, type: "added", object });
  assert.equal(arrival.uid, "u-1", "an added event must name the resource it added");
  assert.equal(arrival.added, true, "an arrival is not a change to an existing object");

  // A value moving is NOT structural: the keys are the same, so a UI re-reads rather than rebuilds.
  const moved = applyStreamEvent(store, {
    seq: 2,
    type: "modified",
    object: { ...object, metadata: { ...object.metadata, resourceVersion: "2" }, data: { greeting: "hi" } },
  });
  assert.equal(moved.uid, "u-1");
  assert.equal(moved.added, false, "a modification of a known uid is not an arrival");
  assert.equal(moved.structural, false, "no key appeared or disappeared");
  // metadata.resourceVersion flashes too — it is a read-only field and it genuinely moved. Assert on
  // the one this test is about rather than pinning the whole set, which would be a test of the
  // flashing rules and those have their own fixtures.
  assert.ok(
    moved.flashed.some((p) => p.join("/") === "data/greeting"),
    `the changed value must flash: ${JSON.stringify(moved.flashed)}`,
  );

  // A key APPEARING is structural: a renderer that only re-reads known values never shows it.
  const grew = applyStreamEvent(store, {
    seq: 3,
    type: "modified",
    object: {
      ...object,
      metadata: { ...object.metadata, resourceVersion: "3" },
      data: { greeting: "hi", farewell: "bye" },
    },
  });
  assert.equal(grew.structural, true, "a new key must be reported as structural, or the row is never drawn");

  // And a delete names the uid too — a host cannot remove a row it cannot identify.
  const gone = applyStreamEvent(store, {
    seq: 4,
    type: "deleted",
    identity: { uid: "u-1", apiVersion: "v1", kind: "ConfigMap", name: "app", namespace: "default" },
  });
  assert.equal(gone.uid, "u-1", "a deleted event must name the resource it removed");
  assert.equal(gone.structural, true, "a row left the collection");
});
