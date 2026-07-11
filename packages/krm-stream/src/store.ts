// LiveResourceStore — the consumer half of the protocol (spec §10) and the engine of
// docs/client-state-model.md.
//
// It holds, per uid:
//
//   server(id)   the authoritative projected object. REPLACED by every added/modified — never
//                deep-merged, because a deep merge of complete objects cannot express a removal and
//                so resurrects the field the server just deleted (spec §4.1). It is also the merge
//                BASE, and it advances on every event (I-BASESHIFT).
//   draft(id)    the same object with the editable regions three-way merged against the user's edits.
//
// Dirtiness is not in that list, on purpose: it is DERIVED, every time it is asked for (R-DERIVED).
// A cached dirty set goes stale on the next watch event — the field the server converged onto stays
// flagged forever, or unflags itself only on the next click.

import { clone, deepEqual, isPlainObject } from "./deep.ts";
import { get, has, isPrefix, parsePointer, pathKey, removeAt, setAt } from "./path.ts";
import { reconcile, type MergeState, type Regions } from "./merge.ts";
import { defaultPolicy } from "./policy.ts";
import type { Change, Conflict, EditabilityPolicy, KRMObject, Path } from "./types.ts";

/** What one server event did, for a host that wants to animate it. `flashed` is an OUTPUT, not
 * state: the host highlights those paths and forgets them. */
export interface ApplyResult {
  /** The uid was not known: this is an arrival, not a change. */
  added: boolean;
  /** Keys or array elements appeared or disappeared — a UI must rebuild rows, not just re-read values. */
  structural: boolean;
  /** Paths the server moved, in read-only regions and in editable regions the user had not touched. */
  flashed: Path[];
  /** The paths this object is now conflicted at (the complete set, not just the new ones). */
  conflicts: Path[];
}

export interface ApplyOptions {
  /** RFC 6901 JSON Pointers (`"/data/token"`, as they arrive on the wire) or segment arrays. The
   * paths this object's values were masked at: read-only, never dirtiable, never in a patch. */
  redactedPaths?: (string | Path)[];
}

interface Resource {
  server: KRMObject;
  draft: KRMObject;
  redacted: Path[];
  conflicts: Map<string, Conflict>;
}

export class LiveResourceStore {
  readonly #policy: EditabilityPolicy;
  readonly #resources = new Map<string, Resource>();
  readonly #subscribers = new Set<() => void>();

  /** Non-null exactly while a snapshot cycle is open: the uids seen since `beginSnapshot()`.
   * Pruning reads it in `endSnapshot()` and NOWHERE else — a cycle that never completes must prune
   * nothing, or a network hiccup makes the user watch half their resources evaporate (spec §5). */
  #seen: Set<string> | null = null;

  constructor(policy: EditabilityPolicy = defaultPolicy) {
    this.#policy = policy;
  }

  // ------------------------------------------------------------------ the stream in --

  /** `added` and `modified` — the only two upsert spellings, and they are treated identically
   * (spec §4). Both mean "here is this object's complete current state". */
  applyServerEvent(object: KRMObject, opts: ApplyOptions = {}): ApplyResult {
    const id = object.metadata.uid;
    const incoming = clone(object);
    const redacted = (opts.redactedPaths ?? []).map((p) => (typeof p === "string" ? parsePointer(p) : [...p]));

    this.#seen?.add(id);

    const existing = this.#resources.get(id);
    if (!existing) {
      // A resource we have never seen: base = draft = incoming. There is nothing to merge, and
      // nothing flashes — an arrival is not a change.
      this.#resources.set(id, { server: incoming, draft: clone(incoming), redacted, conflicts: new Map() });
      this.#notify();
      return { added: true, structural: true, flashed: [], conflicts: [] };
    }

    const state: MergeState = {
      regions: this.#regionsFor(incoming, redacted),
      conflicts: existing.conflicts,
      flashed: [],
    };
    const merged = reconcile(state, existing.server, existing.draft, incoming) as KRMObject;

    const structural = !sameShape(existing.draft, merged);
    // The REPLACEMENT (spec §4.1) and the base shift (I-BASESHIFT), in one line. Everything above
    // reconciled against the OLD server object; from here on, `incoming` is the base.
    existing.server = incoming;
    existing.draft = merged;
    existing.redacted = redacted;

    this.#notify();
    return {
      added: false,
      structural,
      flashed: state.flashed,
      conflicts: [...existing.conflicts.values()].map((c) => c.path),
    };
  }

  /** `deleted`. The object is gone, and so is any draft of it — the user was editing something that
   * no longer exists. (A recreate under the same name is a DIFFERENT uid and starts clean; that is
   * the whole reason identity is the uid.) */
  removeResource(id: string): void {
    if (this.#resources.delete(id)) this.#notify();
  }

  /** `reset`. Mark every known uid unseen — and prune NOTHING yet. */
  beginSnapshot(): void {
    this.#seen = new Set();
  }

  /** `synced`. The snapshot is complete, so what it did not mention is genuinely gone. This is the
   * only place anything is pruned, and it is what removes an object deleted while the consumer was
   * disconnected — the one event the consumer never saw. */
  endSnapshot(): void {
    const seen = this.#seen;
    if (!seen) return; // a `synced` with no open cycle: ignore, do not prune the world
    this.#seen = null;
    let pruned = false;
    for (const id of [...this.#resources.keys()]) {
      if (!seen.has(id)) {
        this.#resources.delete(id);
        pruned = true;
      }
    }
    if (pruned) this.#notify();
  }

  /** The save succeeded and this is the object it produced. The watch will echo it too — and that
   * echo is a harmless no-op (I-IDEMPOTENT) — but a UI should not have to wait for it to stop
   * showing the field as dirty. */
  adoptSaved(object: KRMObject): void {
    const existing = this.#resources.get(object.metadata.uid);
    if (!existing) {
      this.applyServerEvent(object);
      return;
    }
    existing.server = clone(object);
    existing.draft = clone(object);
    existing.conflicts.clear();
    this.#notify();
  }

  // ------------------------------------------------------------------------- edits --

  setValue(id: string, path: Path, value: unknown): void {
    const res = this.#editable(id, path);
    setAt(res.draft, path, clone(value));
    this.#settle(res, path);
  }

  /** For a map entry or an object key the user deleted. It stays deleted across watch events (the
   * merge sees ours=undefined, base=theirs and keeps the deletion) and becomes a `null` in the patch. */
  removeKey(id: string, path: Path): void {
    const res = this.#editable(id, path);
    removeAt(res.draft, path);
    this.#settle(res, path);
  }

  /** `path` addresses the MAP; `key` is the new entry. A new row in a UI starts empty. */
  addKey(id: string, path: Path, key: string, value: unknown = ""): void {
    const res = this.#editable(id, [...path, key]);
    setAt(res.draft, [...path, key], clone(value));
    this.#settle(res, [...path, key]);
  }

  /** `path` addresses the MAP. Order is preserved — renaming a label must not make its row jump to
   * the bottom of the list while the user is typing in it. */
  renameKey(id: string, path: Path, oldKey: string, newKey: string): void {
    const res = this.#editable(id, [...path, oldKey]);
    this.#editable(id, [...path, newKey]);
    const map = get(res.draft, path);
    if (!isPlainObject(map)) throw new Error(`krm-stream: ${pathKey(path)} is not a map`);
    const renamed: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(map)) renamed[k === oldKey ? newKey : k] = v;
    setAt(res.draft, path, renamed);
    this.#settle(res, [...path, oldKey]);
    this.#settle(res, [...path, newKey]);
  }

  /** Throw the local edit away and take the server's value — which is also how a conflict is
   * resolved in the server's favour. */
  revert(id: string, path: Path): void {
    const res = this.#editable(id, path);
    if (has(res.server, path)) setAt(res.draft, path, clone(get(res.server, path)));
    else removeAt(res.draft, path);
    this.#settle(res, path);
  }

  /** Resolve a conflict by keeping the server's value. */
  takeTheirs(id: string, path: Path): void {
    this.revert(id, path);
  }

  // ----------------------------------------------------------------------- queries --

  ids(): string[] {
    return [...this.#resources.keys()];
  }

  /** The authoritative server object, including live `status`. */
  server(id: string): KRMObject {
    return clone(this.#must(id).server);
  }

  /** The server object with the editable regions merged over it. What a UI renders and edits. */
  draft(id: string): KRMObject {
    return clone(this.#must(id).draft);
  }

  /** Read-only convenience for the watch UI. A kind with no status simply has none. */
  status(id: string): unknown {
    return clone(this.#must(id).server["status"]);
  }

  /** Every pending edit, derived fresh by comparing the draft to the server object. Arrays appear
   * whole (§4.1), which is also what RFC 7386 wants. */
  changes(id: string): Change[] {
    const res = this.#must(id);
    const out: Change[] = [];
    this.#diff(this.#regionsFor(res.server, res.redacted), [], res.server, res.draft, out);
    return out;
  }

  /** R-DERIVED: never a stored flag. A read-only path is never dirty by construction. */
  isDirty(id: string, path: Path): boolean {
    const res = this.#must(id);
    if (this.isEditable(id, path)) return !deepEqual(get(res.server, path), get(res.draft, path));
    // A container (`[]`, `["metadata"]`) is dirty iff something editable underneath it is.
    if (!this.#policy.containsEditable(res.server, path)) return false;
    return this.changes(id).some((c) => isPrefix(path, c.path));
  }

  conflicts(id: string): Conflict[] {
    return [...this.#must(id).conflicts.values()].map((c) => ({ path: [...c.path], theirs: clone(c.theirs) }));
  }

  /** The policy, minus this object's redacted paths. A masked value is read-only for exactly the
   * same reason `status` is: it is not the user's to change — and here, they never even saw it. */
  isEditable(id: string, path: Path): boolean {
    const res = this.#must(id);
    return this.#regionsFor(res.server, res.redacted).editable(path);
  }

  /** An RFC 7386 merge patch of the editable changes, or null when there is nothing to save.
   *
   * Built from the user's EDITS — never from a diff of the whole object against the server. The
   * object on the wire is a projection: a path the projection removed is simply not there, and a
   * whole-object diff would read that absence as a deletion and try to patch it away (spec §3). */
  patch(id: string): Record<string, unknown> | null {
    const changes = this.changes(id);
    if (changes.length === 0) return null;
    const out: Record<string, unknown> = {};
    for (const c of changes) setAt(out, c.path, c.new === undefined ? null : clone(c.new));
    return out;
  }

  /** A coarse "something changed" signal — the host re-renders and re-queries. Returns an
   * unsubscribe. */
  subscribe(cb: () => void): () => void {
    this.#subscribers.add(cb);
    return () => this.#subscribers.delete(cb);
  }

  // ---------------------------------------------------------------------- internals --

  #must(id: string): Resource {
    const res = this.#resources.get(id);
    if (!res) throw new Error(`krm-stream: no resource ${JSON.stringify(id)}`);
    return res;
  }

  /** Every edit goes through here: a write to `status`, to `metadata.name`, or to a redacted path is
   * refused. The engine is not the security boundary — the gateway rejects such a patch too — but a
   * UI that cannot even form the edit is the difference between "safe" and "safe if the client is
   * honest". */
  #editable(id: string, path: Path): Resource {
    const res = this.#must(id);
    if (!this.isEditable(id, path)) {
      throw new Error(`krm-stream: ${pathKey(path)} is read-only`);
    }
    return res;
  }

  #regionsFor(object: KRMObject, redacted: Path[]): Regions {
    const isRedacted = (path: Path) => redacted.some((r) => isPrefix(r, path));
    return {
      editable: (path) => !isRedacted(path) && this.#policy.isEditable(object, path),
      container: (path) => this.#policy.containsEditable(object, path),
    };
  }

  /** After an edit: a conflict whose path the draft has now brought back to the server's value is no
   * longer a conflict. (Typing the server's value by hand resolves it, and so does `revert`.) */
  #settle(res: Resource, path: Path): void {
    for (const [k, c] of res.conflicts) {
      if (!isPrefix(c.path, path) && !isPrefix(path, c.path)) continue;
      if (deepEqual(get(res.server, c.path), get(res.draft, c.path))) res.conflicts.delete(k);
    }
    this.#notify();
  }

  #diff(regions: Regions, path: Path, srv: unknown, drf: unknown, out: Change[]): void {
    if (regions.editable(path)) {
      const defined = [srv, drf].filter((v) => v !== undefined);
      if (defined.length > 0 && defined.every(isPlainObject)) {
        for (const k of unionKeys(srv, drf)) {
          this.#diff(regions, [...path, k], (srv as never)?.[k], (drf as never)?.[k], out);
        }
        return;
      }
      // Scalars, arrays, and shape changes are one atomic value — the same rule the merge uses, so
      // "what is dirty" and "what was merged atomically" can never disagree.
      if (!deepEqual(srv, drf)) {
        const kind = srv === undefined ? "add" : drf === undefined ? "delete" : "update";
        out.push({ path: [...path], kind, old: clone(srv), new: clone(drf) });
      }
      return;
    }
    if (regions.container(path)) {
      for (const k of unionKeys(srv, drf)) {
        this.#diff(regions, [...path, k], (srv as never)?.[k], (drf as never)?.[k], out);
      }
    }
    // Read-only: the draft IS the server there. Nothing to diff, nothing to save.
  }

  #notify(): void {
    for (const cb of this.#subscribers) cb();
  }
}

function unionKeys(...values: unknown[]): string[] {
  const keys: string[] = [];
  const seen = new Set<string>();
  for (const v of values) {
    if (!isPlainObject(v)) continue;
    for (const k of Object.keys(v)) {
      if (!seen.has(k)) {
        seen.add(k);
        keys.push(k);
      }
    }
  }
  return keys;
}

/** Did keys or array elements appear or disappear? A UI can re-read values cheaply; it must REBUILD
 * when rows come and go — that is what `structural` tells it. Scalar value changes are not structural. */
function sameShape(a: unknown, b: unknown): boolean {
  if (isPlainObject(a) && isPlainObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    if (ka.length !== kb.length) return false;
    return ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && sameShape(a[k], b[k]));
  }
  if (Array.isArray(a) && Array.isArray(b)) {
    return a.length === b.length && a.every((x, i) => sameShape(x, b[i]));
  }
  return isPlainObject(a) === isPlainObject(b) && Array.isArray(a) === Array.isArray(b);
}
