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
      flashed.push(...applyStreamEvent(store, resolve(f, fe)));

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
