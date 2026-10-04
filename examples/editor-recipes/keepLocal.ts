import type { LiveResourceStore, Path } from "../../packages/krm-stream/src/index.ts";

/** Copy into the host. Resolves the conflict at `path` in favour of the person's local value — the
 * opposite of `store.revert(uid, path)`, which takes the server's.
 *
 * The store keeps the local value in the draft and records the conflict until someone decides. To
 * keep it: capture the local value (including its absence), let `revert` adopt the server's value and
 * clear the conflict, then put the local value back. Both steps run in one synchronous call, so no
 * stream event can land between them. The draft then differs from the server again — an ordinary
 * edit, no longer a conflict — and the next `captureSave` binds it to the server version the person
 * reviewed.
 *
 * Returns false, changing nothing, when the path is not editable: a policy or a redaction (which can
 * appear after the conflict did) makes it read-only. Unrelated edits and conflicts are untouched; a
 * conflict nested under `path` is resolved with it, because the local subtree is kept whole. Arrays
 * are atomic, so keeping a local array keeps the whole array. */
export function keepLocal(store: LiveResourceStore, uid: string, path: Path): boolean {
  if (!store.isEditable(uid, path)) return false;
  const draft = store.draft(uid);
  const present = has(draft, path);
  const local = present ? get(draft, path) : undefined;
  store.revert(uid, path);
  if (present) store.setValue(uid, path, local);
  else if (has(store.draft(uid), path)) store.removeKey(uid, path);
  return true;
}

function get(value: unknown, path: Path): unknown {
  let cur = value;
  for (const segment of path) {
    if (cur === null || typeof cur !== "object") return undefined;
    cur = (cur as Record<string | number, unknown>)[segment];
  }
  return cur;
}

function has(value: unknown, path: Path): boolean {
  let cur = value;
  for (const segment of path) {
    if (cur === null || typeof cur !== "object" || !Object.hasOwn(cur, segment)) return false;
    cur = (cur as Record<string | number, unknown>)[segment];
  }
  return true;
}
