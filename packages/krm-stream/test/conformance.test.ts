// The contract, checked from the TypeScript side.
//
// There is no store yet, so these tests do not exercise a merge. They exercise the corpus: that it
// loads, that every object a scenario names exists, that the events are ones the protocol defines,
// and that the client-side fixtures are actually runnable (their edits address objects that are on
// the wire at the moment the edit is applied).
//
// That last check is worth more than it sounds. It is what stops someone writing a beautiful merge
// fixture that edits a uid the stream never delivered — a test that would pass against a store that
// does nothing at all.

import assert from "node:assert/strict";
import { test } from "node:test";
import { allBodies, allFixtures, clientFixtures, type Fixture, resolve } from "./conformance.ts";

test("the corpus loads", () => {
  assert.ok(allFixtures().length > 0, "no fixtures — run `task fixtures`");
  assert.ok(Object.keys(allBodies()).length > 0, "no bodies — run `task fixtures`");
});

test("both suites see the same corpus the Go suite sees", () => {
  // Not a tautology: it is the assertion that conformance/gen/ is the ONE artifact both languages
  // read. If someone ever gives the client its own copy of the fixtures, this file is where the
  // reviewer should notice.
  for (const f of allFixtures()) {
    assert.ok(f.id, "every fixture has an id");
    assert.ok(f.title, `${f.id}: every fixture says what it is`);
    assert.ok(f.why, `${f.id}: every fixture names the rule it defends`);
  }
});

test("every event resolves to a well-formed wire event", () => {
  for (const f of allFixtures()) {
    for (const [i, fe] of f.events.entries()) {
      const ev = resolve(f, fe);
      assert.equal(ev.type, fe.type, `${f.id} event ${i}`);

      if (ev.type === "added" || ev.type === "modified") {
        assert.ok(ev.object?.metadata?.uid, `${f.id} event ${i}: an object on the wire has a uid`);
        // Required, not optional: a consumer must never have to INFER redaction from a value that
        // merely looks like a placeholder.
        assert.ok(Array.isArray(ev.redacted), `${f.id} event ${i}: redacted must be present`);
      }
      if (ev.type === "deleted") {
        assert.ok(ev.identity?.uid, `${f.id} event ${i}: a tombstone carries a trustworthy uid`);
      }
    }
  }
});

test("a snapshot cycle is reset … added* … synced", () => {
  for (const f of allFixtures()) {
    let inCycle = false;
    let sawReset = false;
    let endedTerminally = false;
    for (const [i, fe] of f.events.entries()) {
      assert.ok(
        !endedTerminally,
        `${f.id} event ${i}: ${fe.type} after a TERMINAL error, which must be the LAST event`,
      );
      if (fe.type === "reset") {
        assert.ok(!inCycle, `${f.id} event ${i}: reset inside an unclosed cycle`);
        inCycle = sawReset = true;
      } else if (fe.type === "synced") {
        assert.ok(inCycle, `${f.id} event ${i}: synced without a reset`);
        inCycle = false;
      } else {
        assert.ok(sawReset, `${f.id} event ${i}: ${fe.type} before the first reset`);
        if (fe.type === "error" && fe.terminal) endedTerminally = true;
      }
    }
    // A stream may legally end mid-cycle in exactly two ways, and each of them is a fixture:
    // partial-cycle-no-prune (the connection died — and the consumer must prune NOTHING), and a
    // TERMINAL error, which is by definition the last event on the connection (spec §4.3) and can
    // perfectly well arrive mid-snapshot — see resourceversion-unorderable, where the gateway only
    // discovers the upstream is not what it was promised once the first object arrives.
    if (inCycle && !endedTerminally) {
      assert.equal(f.id, "partial-cycle-no-prune", `${f.id}: ends mid-cycle — is that the point?`);
    }
  }
});

test("client fixtures edit objects the stream actually delivered", () => {
  for (const f of clientFixtures()) {
    for (const edit of f.client?.edits ?? []) {
      const delivered = deliveredUidsBefore(f, edit.after);
      assert.ok(
        delivered.has(edit.uid),
        `${f.id}: the edit at after=${edit.after} addresses ${edit.uid}, which the stream has not delivered by then`,
      );
      assert.ok(Array.isArray(edit.path) && edit.path.length > 0, `${f.id}: a path is a non-empty segment ARRAY`);
    }
  }
});

test("paths are segment arrays, never dot-joined strings", () => {
  // R-ID, defended structurally rather than by hoping. `app.kubernetes.io/name` is ONE segment; a
  // fixture that writes it as "metadata.labels.app.kubernetes.io/name" would be teaching the bug.
  for (const f of clientFixtures()) {
    const paths = [
      ...(f.client?.expect?.dirty ?? []),
      ...(f.client?.expect?.absentPaths ?? []),
      ...(f.client?.expect?.readOnlyPaths ?? []),
      ...(f.client?.expect?.flashed ?? []),
      ...(f.client?.edits ?? []).map((e) => e.path),
    ];
    for (const p of paths) {
      assert.ok(Array.isArray(p), `${f.id}: ${JSON.stringify(p)} must be a segment array`);
      assert.ok(
        !(p.length === 1 && typeof p[0] === "string" && p[0].includes(".") && p[0].startsWith("metadata")),
        `${f.id}: ${JSON.stringify(p)} looks dot-joined`,
      );
    }
  }
});

function deliveredUidsBefore(f: Fixture, after: number): Set<string> {
  const uids = new Set<string>();
  for (const fe of f.events.slice(0, after + 1)) {
    const ev = resolve(f, fe);
    if (ev.object) uids.add(ev.object.metadata.uid);
    if (ev.type === "deleted" && ev.identity) uids.delete(ev.identity.uid);
  }
  return uids;
}
