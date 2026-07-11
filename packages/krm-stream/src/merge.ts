// The deep three-way reconcile (docs/client-state-model.md §4).
//
//   base   the PREVIOUS server object we last reconciled from
//   ours   the user's working draft
//   theirs the complete incoming server object
//
// The base is the whole point (R-THREEWAY). Compare `theirs` to `ours` instead and you cannot tell
// "the server changed this" from "the user typed this", so every watch event looks like the server
// changed everything the user touched — and a controller's status heartbeat false-conflicts an edit
// three fields away. Only a path where base ≠ theirs was actually moved by the server.
//
// Note what is NOT here: replacing the authoritative server object. That happens in the store, and
// it is a REPLACEMENT, never a merge (spec §4.1) — merging complete server objects resurrects the
// fields the server just removed. These are two layers, and conflating them is the ghost bug.

import { clone, deepEqual, isPlainObject } from "./deep.ts";
import { at, pathKey } from "./path.ts";
import type { Conflict, Path } from "./types.ts";

/** The store's per-resource view of which paths are editable — the policy, minus the paths this
 * particular object declared redacted. */
export interface Regions {
  /** This path is inside an editable region: three-way merge it. */
  editable(path: Path): boolean;
  /** Not editable itself, but an editable region lives underneath: recurse, don't replace. */
  container(path: Path): boolean;
}

export interface MergeState {
  regions: Regions;
  /** Conflicts persist between events — a conflict the next event does not touch is still a
   * conflict. (Dirtiness is the opposite: derived, never stored. R-DERIVED.) */
  conflicts: Map<string, Conflict>;
  /** An output, not state: the paths the server moved. The host flashes them and forgets them. */
  flashed: Path[];
}

/** Reconcile one resource. Returns the new draft. */
export function reconcile(s: MergeState, base: unknown, ours: unknown, theirs: unknown): unknown {
  return node(s, [], base, ours, theirs);
}

function node(s: MergeState, path: Path, base: unknown, ours: unknown, theirs: unknown): unknown {
  if (s.regions.editable(path)) return mergeEditable(s, path, base, ours, theirs);
  if (s.regions.container(path)) return mergeContainer(s, path, base, ours, theirs);
  return follow(s, path, base, theirs);
}

/** A read-only region: take the server's value, flash what moved, and never look at the draft.
 * There is no draft here to look at — that is what read-only MEANS — but it still updates live. */
function follow(s: MergeState, path: Path, base: unknown, theirs: unknown): unknown {
  if (isPlainObject(base) && isPlainObject(theirs)) {
    const out: Record<string, unknown> = {};
    for (const k of unionKeys(base, theirs)) {
      const v = follow(s, [...path, k], at(base, k), at(theirs, k));
      if (v !== undefined) out[k] = v;
    }
    return out; // structurally clone(theirs): a key `theirs` lacks is GONE, not retained
  }
  // Arrays flash atomically, at the array's own path — a UI highlights "the conditions changed",
  // not "conditions[0].reason changed", and §4.1 treats arrays as one value anyway.
  if (!deepEqual(base, theirs)) s.flashed.push(path);
  return clone(theirs);
}

/** Not editable, but an editable region is somewhere below (`[]`, `["metadata"]`). Recurse and let
 * each child dispatch for itself: `metadata.labels` is merged while `metadata.name` beside it
 * follows the server. */
function mergeContainer(s: MergeState, path: Path, base: unknown, ours: unknown, theirs: unknown): unknown {
  if (!isPlainObject(base) && !isPlainObject(ours) && !isPlainObject(theirs)) {
    return follow(s, path, base, theirs);
  }
  return recurse(s, path, base, ours, theirs);
}

function mergeEditable(s: MergeState, path: Path, base: unknown, ours: unknown, theirs: unknown): unknown {
  const defined = [base, ours, theirs].filter((v) => v !== undefined);
  if (defined.length > 0 && defined.every(isPlainObject)) return recurse(s, path, base, ours, theirs);

  // Everything else — scalars, arrays, and any node where the three disagree about their very shape
  // — is ONE atomic value.
  //
  // For arrays this is docs §4.1, and it is coarse on purpose: a positional merge of an array whose
  // length moved mis-aligns (a PREPENDED sidecar would merge the user's edit into the wrong
  // container), so a dirty array is treated as a single value and the user's version wins until they
  // resolve it. RFC 7386 replaces arrays wholesale too, so the patch format needs no special case.
  if (deepEqual(base, ours)) {
    // The user has no edit here. Follow the server — including following it into deletion.
    if (!deepEqual(base, theirs)) s.flashed.push(path);
    clearConflict(s, path);
    return clone(theirs);
  }
  // Dirty: `ours` diverged from `base`. The user's value is never silently overwritten.
  if (deepEqual(base, theirs)) return clone(ours); // the server did not move it — no conflict (I-NOFALSE)
  if (deepEqual(ours, theirs)) {
    clearConflict(s, path); // the server arrived at what the user typed (I-CONVERGE)
    return clone(ours);
  }
  setConflict(s, path, theirs);
  return clone(ours);
}

function recurse(s: MergeState, path: Path, base: unknown, ours: unknown, theirs: unknown): unknown {
  const out: Record<string, unknown> = {};
  for (const k of unionKeys(base, ours, theirs)) {
    const v = node(s, [...path, k], at(base, k), at(ours, k), at(theirs, k));
    if (v !== undefined) out[k] = v; // prune: a key that reconciled to nothing is GONE
  }
  // The server removed this whole subtree and nothing of the user's survived in it. Returning `{}`
  // here would leave an empty container the server does not have — which then reads as a dirty
  // "the user added an empty map" forever. (`theirs === {}` is a real empty map and is kept.)
  if (theirs === undefined && Object.keys(out).length === 0) return undefined;
  return out;
}

function unionKeys(...values: unknown[]): string[] {
  const keys: string[] = [];
  const seen = new Set<string>();
  for (const v of values) {
    if (!isPlainObject(v)) continue;
    for (const k of Object.keys(v)) {
      if (seen.has(k)) continue;
      seen.add(k);
      keys.push(k);
    }
  }
  return keys;
}

function setConflict(s: MergeState, path: Path, theirs: unknown): void {
  s.conflicts.set(pathKey(path), { path: [...path], theirs: clone(theirs) });
}

function clearConflict(s: MergeState, path: Path): void {
  s.conflicts.delete(pathKey(path));
}
