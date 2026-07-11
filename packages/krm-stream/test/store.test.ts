// The client's half of the conformance corpus, actually asserted.
//
// conformance.test.ts checks that the corpus is well-formed. This file checks that the STORE is:
// every client fixture is replayed event by event into a LiveResourceStore, the fixture's edits are
// applied at their `after:` positions, and what the store then holds is compared to `expect:`.
//
// The `after:` ordering is the whole point. An edit placed BEFORE the server change that collides
// with it is the only way to test a three-way merge at all — a suite that edited only at the end
// would pass against a store that just follows the server.
//
// Assertion strengths, per conformance/README.md:
//   uids, dirty, conflicts   exact sets (order-insensitive)
//   patch                    exact — it is what gets sent to the API server
//   draftSubset              deep subset (arrays compared element-wise, same length)
//   absentPaths              the path is absent from BOTH server(id) and draft(id)
//   readOnlyPaths            not editable, not dirty, and setValue on it is refused
//   flashed                  containment: every listed path flashed. NOT an exact set — a read-only
//                            region that changed flashes, and metadata.resourceVersion changes on
//                            EVERY event, so an exact set would assert that a conformant engine
//                            does not flash a read-only field that moved. See docs §3 ("read-only
//                            is not ignored"). The fixtures list the paths a UI must highlight.

import assert from "node:assert/strict";
import { test } from "node:test";
import { LiveResourceStore } from "../src/index.ts";
import type { Path, StreamEvent } from "../src/types.ts";
import { clientFixtures, type FixtureEdit, type FixtureExpect, resolve } from "./conformance.ts";

for (const f of clientFixtures()) {
  test(`${f.id}: ${f.title}`, () => {
    const store = new LiveResourceStore();
    const flashed: Path[] = [];

    for (const [i, fe] of f.events.entries()) {
      feed(store, resolve(f, fe), flashed);

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

/** The four protocol events a store cares about. `error` is a transport concern — the store holds
 * state, not a connection — and an unknown type must be ignored, not thrown on (spec §0). */
function feed(store: LiveResourceStore, ev: StreamEvent, flashed: Path[]): void {
  switch (ev.type) {
    case "reset":
      store.beginSnapshot();
      break;
    case "added":
    case "modified":
      flashed.push(...store.applyServerEvent(ev.object!, { redactedPaths: ev.redactedPaths }).flashed);
      break;
    case "deleted":
      store.removeResource(ev.identity!.uid);
      break;
    case "synced":
      store.endSnapshot();
      break;
    default:
      break;
  }
}

/** `path` in a fixture edit always addresses the FIELD, not its container — even for the two ops
 * whose store signature takes the map plus a key. Keeping the fixture format uniform is worth the
 * one line of translation here. */
function applyEdit(store: LiveResourceStore, e: FixtureEdit): void {
  const parent = e.path.slice(0, -1);
  const last = e.path[e.path.length - 1];
  switch (e.op) {
    case "set":
      store.setValue(e.uid, e.path, e.value);
      break;
    case "remove":
      store.removeKey(e.uid, e.path);
      break;
    case "addKey":
      store.addKey(e.uid, parent, String(last), e.value);
      break;
    case "renameKey":
      store.renameKey(e.uid, parent, String(last), e.newKey!);
      break;
    case "revert":
      store.revert(e.uid, e.path);
      break;
    default:
      throw new Error(`fixture: unknown edit op ${JSON.stringify(e.op)}`);
  }
}

function check(store: LiveResourceStore, flashed: Path[], want: FixtureExpect, where: string): void {
  const ids = store.ids();

  if (want.uids) assert.deepEqual(sorted(ids), sorted(want.uids), `${where}: uids`);

  // Dirtiness is derived, so we ask for it the way a UI would: the changes of every resource the
  // store holds. Union, because no fixture edits two objects and a stray dirty path anywhere is a bug.
  if (want.dirty) {
    const dirty = ids.flatMap((id) => store.changes(id).map((c) => c.path));
    assert.deepEqual(keys(dirty), keys(want.dirty), `${where}: dirty paths`);
    // R-DERIVED, from the other direction: the predicate and the change list must agree.
    for (const p of want.dirty) {
      assert.ok(
        ids.some((id) => store.isDirty(id, p)),
        `${where}: isDirty(${JSON.stringify(p)}) disagrees with changes()`,
      );
    }
  }

  if (want.conflicts) {
    const got = ids.flatMap((id) => store.conflicts(id));
    assert.deepEqual(keys(got.map((c) => c.path)), keys(want.conflicts.map(conflictPath)), `${where}: conflict paths`);
    for (const c of want.conflicts) {
      if (Array.isArray(c) || c.theirs === undefined) continue; // the fixture pins the path only
      const found = got.find((g) => key(g.path) === key(c.path));
      assert.deepEqual(found?.theirs, c.theirs, `${where}: conflict theirs at ${key(c.path)}`);
    }
  }

  const subject = subjectId(store);

  if (want.draftSubset) {
    const drafts = subject ? [subject] : ids;
    const ok = drafts.some((id) => isSubset(store.draft(id) as unknown, want.draftSubset));
    assert.ok(
      ok,
      `${where}: no draft matches draftSubset\nwant: ${JSON.stringify(want.draftSubset, null, 2)}\ngot:  ${JSON.stringify(
        drafts.map((id) => store.draft(id)),
        null,
        2,
      )}`,
    );
  }

  for (const p of want.absentPaths ?? []) {
    const id = subject ?? ids[0]!;
    assert.equal(has(store.server(id), p), false, `${where}: ${key(p)} is a GHOST in server(${id})`);
    assert.equal(has(store.draft(id), p), false, `${where}: ${key(p)} is a GHOST in draft(${id})`);
  }

  for (const p of want.readOnlyPaths ?? []) {
    const id = subject ?? ids[0]!;
    assert.equal(store.isEditable(id, p), false, `${where}: ${key(p)} must be read-only`);
    assert.equal(store.isDirty(id, p), false, `${where}: ${key(p)} is read-only and can never be dirty`);
    assert.throws(() => store.setValue(id, p, "hunter2"), `${where}: a write to ${key(p)} must be refused`);
  }

  for (const p of want.flashed ?? []) {
    assert.ok(keys(flashed).includes(key(p)), `${where}: ${key(p)} changed on the server and must flash`);
  }

  if (want.patch !== undefined) {
    // No subject means no edits and more than one resource: then NOTHING may have a patch.
    const targets = subject ? [subject] : ids;
    for (const id of targets) assert.deepEqual(store.patch(id), want.patch, `${where}: patch(${id})`);
  }
}

/** The resource a per-object expectation (draftSubset, absentPaths, patch) is about. When the store
 * holds exactly one, it is that one — which is every fixture that asserts a patch, including
 * delete-recreate-uid, where the edited object no longer exists and the survivor is the subject.
 * Otherwise there is no subject and the expectation must hold across the board. */
function subjectId(store: LiveResourceStore): string | undefined {
  const ids = store.ids();
  return ids.length === 1 ? ids[0] : undefined;
}

/** Deep subset: every key the fixture names must match. Arrays are compared element-wise AND must
 * be the same length — "the user's array is preserved intact" is exactly what array-atomic-on-change
 * is asserting, and a prefix match would not see a server-appended sidecar leaking into the draft. */
function isSubset(actual: unknown, want: unknown): boolean {
  if (Array.isArray(want)) {
    if (!Array.isArray(actual) || actual.length !== want.length) return false;
    return want.every((w, i) => isSubset(actual[i], w));
  }
  if (want && typeof want === "object") {
    if (!actual || typeof actual !== "object" || Array.isArray(actual)) return false;
    const a = actual as Record<string, unknown>;
    return Object.entries(want as Record<string, unknown>).every(([k, v]) => k in a && isSubset(a[k], v));
  }
  return Object.is(actual, want);
}

function has(obj: unknown, path: Path): boolean {
  let cur: unknown = obj;
  for (const seg of path) {
    if (cur === null || typeof cur !== "object") return false;
    if (!(String(seg) in (cur as Record<string, unknown>))) return false;
    cur = (cur as Record<string, unknown>)[String(seg)];
  }
  return cur !== undefined;
}

const conflictPath = (c: Path | { path: Path }): Path => (Array.isArray(c) ? c : c.path);
const key = (p: Path): string => JSON.stringify(p);
const keys = (ps: Path[]): string[] => ps.map(key).sort();
const sorted = (xs: string[]): string[] => [...xs].sort();

// A suite that silently runs zero fixtures is the failure mode this whole repo exists to prevent.
test("the client suite actually ran the corpus", () => {
  assert.ok(clientFixtures().length >= 10, "the client half of the corpus is missing fixtures");
});
