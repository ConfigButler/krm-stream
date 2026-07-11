// Structural comparison and cloning of JSON values. No dependencies, and deliberately not
// `structuredClone`/`JSON.stringify` — see below.

/** A plain JSON object — an object that is not an array and not null. Everything a KRM object
 * contains is one of: plain object, array, string, number, boolean, null. */
export function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/** Recursive, KEY-ORDER-INDEPENDENT structural equality (I-ORDER-EQ).
 *
 * `JSON.stringify(a) === JSON.stringify(b)` is the tempting one-liner and it is wrong: an API
 * server may serialize the same object with its keys in a different order, and stringify-equality
 * then reports a change that did not happen — which flashes the UI, and worse, makes an untouched
 * field look like it "moved" so a three-way merge stops leaving edits alone. */
export function deepEqual(a: unknown, b: unknown): boolean {
  if (Object.is(a, b)) return true;

  if (Array.isArray(a) || Array.isArray(b)) {
    if (!Array.isArray(a) || !Array.isArray(b) || a.length !== b.length) return false;
    return a.every((x, i) => deepEqual(x, b[i]));
  }

  if (isPlainObject(a) && isPlainObject(b)) {
    const ka = Object.keys(a);
    const kb = Object.keys(b);
    if (ka.length !== kb.length) return false;
    return ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && deepEqual(a[k], b[k]));
  }

  return false;
}

/** A deep copy of a JSON value. The store never hands out a reference into its own state, and never
 * keeps a reference into a caller's object: a draft that aliases the server snapshot would make the
 * base drift silently as the user types, and the three-way merge would compare an object to itself. */
export function clone<T>(v: T): T {
  if (Array.isArray(v)) return v.map(clone) as unknown as T;
  if (isPlainObject(v)) {
    const out: Record<string, unknown> = {};
    for (const [k, x] of Object.entries(v)) out[k] = clone(x);
    return out as T;
  }
  return v;
}
