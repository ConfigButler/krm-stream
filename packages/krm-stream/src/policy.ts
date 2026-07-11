// Which regions of a KRM object a user may edit.
//
// Read-only is NOT ignored (docs §3). A read-only region still follows the server live and still
// flashes what changed — that is the entire live-status-watch use case, and it is the product. It
// simply never grows a draft, a dirty flag, or a conflict, and never appears in a patch.

import { isPrefix } from "./path.ts";
import type { EditabilityPolicy, KRMObject, Path } from "./types.ts";

/** The default editable regions. Note that two of them are at the object ROOT: `ConfigMap.data` and
 * `Secret.data`/`stringData` carry editable payload outside `spec`, and an engine that assumes
 * everything editable lives under `spec` cannot edit a ConfigMap at all — which is the one kind the
 * first consumer edits most. `binaryData` is deliberately absent: it is base64 of arbitrary bytes,
 * and a text input silently corrupts it. */
export const DEFAULT_EDITABLE_REGIONS: Path[] = [
  ["spec"],
  ["metadata", "labels"],
  ["metadata", "annotations"],
  ["data"],
  ["stringData"],
];

/** A policy from a list of editable region roots. Everything under a root is editable; everything
 * else follows the server. */
export function regionPolicy(roots: Path[]): EditabilityPolicy {
  return {
    isEditable: (_obj: KRMObject, path: Path) => roots.some((root) => isPrefix(root, path)),
    // A path that is not editable itself but that an editable region lives UNDER — `[]` and
    // `["metadata"]` for the defaults. The merge must recurse through these rather than replacing
    // them wholesale, or an edit to `metadata.labels` would be clobbered by the read-only handling
    // of `metadata.name` sitting beside it.
    containsEditable: (_obj: KRMObject, path: Path) =>
      roots.some((root) => root.length > path.length && isPrefix(path, root)),
  };
}

/** `spec`, `metadata.labels`/`annotations`, `data`, `stringData` editable; `status`, the rest of
 * `metadata`, `apiVersion`, `kind`, `binaryData` read-only. */
export const defaultPolicy: EditabilityPolicy = regionPolicy(DEFAULT_EDITABLE_REGIONS);

/** Everything read-only: the status-watch use case. Same engine, same stream, no draft — a viewer
 * that cannot accidentally offer to save. */
export const readOnlyPolicy: EditabilityPolicy = regionPolicy([]);
