// Paths: arrays of segments, never dot-joined strings (R-ID).
//
// `["metadata","labels","app.kubernetes.io/name"]` is THREE segments, the last containing two dots
// and a slash. Join it with dots and it splits back into six segments addressing nothing at all —
// no crash, no error, the conflict lookup just silently never matches and the user's edit is lost
// on save. That bug is the reason this module exists and the reason `Path` is not a string.

import { isPlainObject } from "./deep.ts";
import type { Path } from "./types.ts";

/** A scalar key for a path, where a Map needs one. Encoded STRUCTURALLY — never by joining segments
 * with a magic separator, which is how the original bug (a null byte vs a space) got in. */
export function pathKey(path: Path): string {
  return JSON.stringify(path);
}

/** True when `prefix` addresses `path` or an ancestor of it. Segment-wise: `["data"]` is a prefix of
 * `["data","token"]`, and `["dat"]` is a prefix of nothing. */
export function isPrefix(prefix: Path, path: Path): boolean {
  if (prefix.length > path.length) return false;
  return prefix.every((seg, i) => String(seg) === String(path[i]));
}

/** The child of a JSON value at one segment, or undefined if there isn't one. Indexing a scalar (or
 * a string, which would happily answer to `.length`) yields undefined, not nonsense. */
export function at(value: unknown, seg: string | number): unknown {
  if (Array.isArray(value)) return value[Number(seg)];
  if (isPlainObject(value)) return value[String(seg)];
  return undefined;
}

export function get(value: unknown, path: Path): unknown {
  let cur = value;
  for (const seg of path) cur = at(cur, seg);
  return cur;
}

/** True when the path is really THERE — as opposed to present with an undefined value. The
 * difference is the whole of `absentPaths`: a ghost is a key that still exists. */
export function has(value: unknown, path: Path): boolean {
  let cur = value;
  for (const seg of path) {
    if (Array.isArray(cur)) {
      if (Number(seg) >= cur.length) return false;
    } else if (isPlainObject(cur)) {
      if (!Object.prototype.hasOwnProperty.call(cur, String(seg))) return false;
    } else {
      return false;
    }
    cur = at(cur, seg);
  }
  return cur !== undefined;
}

/** Set a value at a path, creating the containers it passes through. A numeric segment creates an
 * array, a string segment an object — so `["spec","ports",0]` does the right thing on a fresh spec. */
export function setAt(root: Record<string, unknown>, path: Path, value: unknown): void {
  if (path.length === 0) throw new Error("krm-stream: cannot set the root of an object");
  let cur: Record<string, unknown> | unknown[] = root;
  for (let i = 0; i < path.length - 1; i++) {
    const seg = path[i]!;
    let next = at(cur, seg);
    if (!isPlainObject(next) && !Array.isArray(next)) {
      next = typeof path[i + 1] === "number" ? [] : {};
      assign(cur, seg, next);
    }
    cur = next as Record<string, unknown> | unknown[];
  }
  assign(cur, path[path.length - 1]!, value);
}

/** Remove the key (or array element) a path addresses. Removing what is not there is a no-op, not
 * an error: the server may have removed it first, and a UI must not blow up on that race. */
export function removeAt(root: Record<string, unknown>, path: Path): void {
  if (path.length === 0) throw new Error("krm-stream: cannot remove the root of an object");
  const parent = get(root, path.slice(0, -1));
  const last = path[path.length - 1]!;
  if (Array.isArray(parent)) parent.splice(Number(last), 1);
  else if (isPlainObject(parent)) delete parent[String(last)];
}

function assign(container: Record<string, unknown> | unknown[], seg: string | number, value: unknown): void {
  if (Array.isArray(container)) container[Number(seg)] = value;
  else container[String(seg)] = value;
}

/** An RFC 6901 JSON Pointer -> a segment array. This is the form `redactedPaths` arrives in on the
 * wire (spec §3); inside the engine everything is a segment array.
 *
 * Segments stay STRINGS. `/data/0` on a map whose key is literally "0" must not become the number
 * 0 — a pointer does not carry enough type information to tell those apart, and guessing corrupts
 * exactly the Secret keys this is used to protect. */
export function parsePointer(pointer: string): Path {
  if (pointer === "") return [];
  if (!pointer.startsWith("/")) throw new Error(`krm-stream: not an RFC 6901 pointer: ${JSON.stringify(pointer)}`);
  return pointer
    .slice(1)
    .split("/")
    .map((seg) => seg.replaceAll("~1", "/").replaceAll("~0", "~"));
}
