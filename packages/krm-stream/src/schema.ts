// OpenAPI-backed associative-list support. Kubernetes uses these two extensions for lists whose
// elements have a stable identity, such as containers by `name`. The store only needs this small
// structural subset; fetching schemas and choosing the right resource schema remain host concerns.

import type { EditabilityPolicy, KRMObject, Path } from "./types.ts";

/** The structural OpenAPI subset Kubernetes uses to describe object fields and associative lists. */
export interface KubernetesStructuralSchema {
  properties?: Record<string, KubernetesStructuralSchema>;
  items?: KubernetesStructuralSchema;
  "x-kubernetes-list-type"?: string;
  "x-kubernetes-list-map-keys"?: string[];
}

/** Adds keyed-list merging to an existing editability policy.
 *
 * Pass the structural schema for the exact GroupVersionKind the store receives. Lists without both
 * Kubernetes extensions, malformed lists, and lists whose elements have duplicate/missing keys all
 * continue to use atomic-array behavior. */
export function withOpenAPIKeyedLists(
  policy: EditabilityPolicy,
  schema: KubernetesStructuralSchema,
): EditabilityPolicy {
  return {
    ...policy,
    listMapKeys: (object: KRMObject, path: Path) => policy.listMapKeys?.(object, path) ?? mapKeysAt(schema, path),
  };
}

function mapKeysAt(root: KubernetesStructuralSchema, path: Path): readonly string[] | undefined {
  let schema: KubernetesStructuralSchema | undefined = root;
  for (const segment of path) {
    schema = typeof segment === "number" ? schema?.items : schema?.properties?.[segment];
    if (!schema) return undefined;
  }
  const keys = schema["x-kubernetes-list-map-keys"];
  if (schema["x-kubernetes-list-type"] !== "map" || !keys || keys.length === 0) return undefined;
  return keys;
}
