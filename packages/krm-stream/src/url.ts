import type { Projection, Scope } from "./types.ts";

// The request half of the wire.
//
// The protocol pinned the scope's FIELDS and, until now, not its ENCODING — so every consumer
// hand-rolled `?group=&version=&resource=`, and the gateway hand-rolled a parser, and nothing ever
// compared the two. This function and Go's `gateway.ScopeFromQuery` are the two ends of that seam,
// and `conformance/scopes.yaml` is the contract they are both tested against: what this builds is
// fed, byte for byte, into that parser.

/** A scope as a CALLER supplies it. `target` is optional here (a single-cluster host may never set
 * one) though it is always present on the `reset` event the gateway sends back. */
export type ScopeQuery = Omit<Scope, "target"> & { target?: string; projection?: Projection };

/**
 * Build the URL for a resource stream.
 *
 * ```ts
 * connectResourceStream(resourceStreamURL("/resource-stream/v1", {
 *   version: "v1", resource: "configmaps", namespace: "app",
 * }), store);
 * ```
 *
 * The field order is FIXED — target, group, version, resource, namespace, name, labelSelector — so
 * that the same scope always produces the same URL. That is not tidiness: a URL that varies by key
 * order is one an HTTP cache, a log aggregator and a human diffing two bug reports all see as two
 * different URLs.
 *
 * Note what this function does NOT take: an API-server address. There is nowhere to put one, which
 * is spec §8's promise made structural rather than merely stated — and the gateway REFUSES such a
 * parameter outright if someone appends one by hand.
 */
export function resourceStreamURL(base: string, scope: ScopeQuery): string {
  if (!scope.resource) throw new Error("krm-stream: scope needs a `resource` (the plural, lowercase API name)");
  if (!scope.version)
    throw new Error("krm-stream: scope needs a `version` (there is no default: v1 and v1beta1 differ)");

  const q = new URLSearchParams();
  const set = (k: string, v: string | undefined) => {
    if (v) q.append(k, v);
  };
  set("target", scope.target);
  set("group", scope.group);
  set("version", scope.version);
  set("resource", scope.resource);
  set("namespace", scope.namespace);
  set("name", scope.name);
  set("labelSelector", scope.labelSelector);
  set("projection", scope.projection);

  return `${base}${base.includes("?") ? "&" : "?"}${q.toString()}`;
}
