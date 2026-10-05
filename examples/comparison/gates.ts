// The measurement's two gates, kept apart from measure.ts so they can be tested without a host or a
// cluster (gates.test.ts). A run is measured only if both pass:
//
//   workloadFailure  the workload did everything it planned. A failed write leaves the cluster
//                    unchanged, the stores still match it, and without this gate a run that measured
//                    nothing would be reported as a successful measurement.
//   divergence       every store holds what the cluster holds — and every projected Secret store holds
//                    the version of that Secret's final successful rotation. A projection withholds
//                    the value and keeps the key, so a store that missed a rotation still shows
//                    exactly the cluster's redacted paths; only the version tells them apart.
//
// deepEqual comes from the client's source rather than the library under measurement: it is the
// gate's own comparison, not something being measured.

import { deepEqual } from "../../packages/krm-stream/src/deep.ts";
import type { LiveResourceStore } from "../../packages/krm-stream/src/index.ts";

export type Source = "native" | "full" | "spec" | "full-shared" | "spec-shared";

/** One object as the cluster holds it: the host's /admin/state. */
export interface StateObject {
  kind: string;
  uid: string;
  name: string;
  resourceVersion: string;
  spec?: unknown;
  status?: unknown;
  data?: Record<string, string>;
}

/** What the host's /admin/workload reports a run did. */
export interface WorkloadOutcome {
  planned?: Record<string, number>;
  done?: Record<string, number>;
  errors?: string[];
  /** Per Secret name, the resourceVersion of its last successful rotation. */
  secretRotations?: Record<string, string>;
}

/** Why the workload did not do what it planned, or undefined when it did all of it. */
export function workloadFailure(outcome: WorkloadOutcome | undefined): string | undefined {
  if (outcome?.planned === undefined || outcome.done === undefined) return "the host reported no workload outcome";
  if (outcome.errors?.length) {
    const more = outcome.errors.length > 1 ? ` (and ${outcome.errors.length - 1} more)` : "";
    return `${outcome.errors.length} workload step(s) failed: ${outcome.errors[0]}${more}`;
  }
  for (const [kind, planned] of Object.entries(outcome.planned)) {
    const done = outcome.done[kind] ?? 0;
    if (done !== planned) return `the workload did ${done} of ${planned} planned ${kind} step(s)`;
  }
  return undefined;
}

/** The store methods the gate reads. */
export type StoreView = Pick<LiveResourceStore, "ids" | "server" | "redactions">;

/** One subscriber's store for one collection. */
export interface Feed {
  source: Source;
  resource: "widgets" | "secrets";
  store: StoreView;
}

/** Why a feed's store does not hold what the cluster holds, or undefined when it does. `rotations`
 * is the workload's secretRotations. */
export function divergence(
  feed: Feed,
  state: StateObject[],
  rotations: Record<string, string> = {},
): string | undefined {
  const kind = feed.resource === "widgets" ? "Widget" : "Secret";
  const want = state.filter((o) => o.kind === kind);
  const ids = feed.store.ids();
  if (ids.length !== want.length || !want.every((o) => ids.includes(o.uid))) {
    return `${kind} membership: store ${ids.length}, cluster ${want.length}`;
  }
  const projected = feed.source !== "native";
  const specOnly = feed.source.startsWith("spec");
  for (const o of want) {
    const got = feed.store.server(o.uid);
    if (kind === "Widget") {
      if (!deepEqual(got.spec, o.spec)) return `${o.name}: spec differs`;
      if (specOnly ? got.status !== undefined : !deepEqual(got.status, o.status)) {
        return `${o.name}: status ${specOnly ? "present in a spec projection" : "differs"}`;
      }
    } else if (projected) {
      // Never a value; exactly the cluster's keys, named as redacted paths.
      if (got.data !== undefined) return `${o.name}: a projected Secret carries values`;
      const paths = feed.store
        .redactions(o.uid)
        .map((r) => r.path.join("/"))
        .sort();
      const keys = Object.keys(o.data ?? {})
        .map((k) => `data/${k}`)
        .sort();
      if (!deepEqual(paths, keys)) return `${o.name}: redacted paths ${paths} != ${keys}`;
      // The paths cannot show a rotation; the version can. Intermediate rotations may coalesce (a
      // reconnect's snapshot carries only the latest), but the final one must have arrived.
      const rotated = rotations[o.name];
      if (rotated !== undefined && got.metadata.resourceVersion !== rotated) {
        return `${o.name}: holds version ${got.metadata.resourceVersion}, not its final rotation ${rotated}`;
      }
    } else if (!deepEqual(got.data, o.data)) {
      return `${o.name}: data differs`;
    }
    // A native store holds the cluster's exact version. A projection may legitimately hold an older
    // one: a suppressed update advances the cluster without an event.
    if (!projected && got.metadata.resourceVersion !== o.resourceVersion) return `${o.name}: resourceVersion differs`;
  }
  return undefined;
}
