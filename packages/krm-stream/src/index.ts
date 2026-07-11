// krm-stream — a live, honest window onto Kubernetes resources, in the browser.
//
// The public surface is small on purpose:
//
//   LiveResourceStore      holds server truth + your draft; three-way merges every watch event;
//                          derives dirtiness; tracks conflicts; builds the merge patch.
//   connectResourceStream  a conforming SSE consumer that feeds a store.  (not built yet)
//
// The store is built test-first against conformance/ — the same fixtures the Go gateway runs. See
// ../../../docs/client-state-model.md for the algorithm, ../../../spec/v1.md for the wire, and
// CONTRIBUTING.md for the order of work.
//
// No runtime dependencies, and none of this knows anything about GitOps, Flux, Dex, kcp or
// ConfigButler. It knows KRM.

export { LiveResourceStore } from "./store.ts";
export type { ApplyResult, ApplyOptions } from "./store.ts";

export { defaultPolicy, readOnlyPolicy, regionPolicy, DEFAULT_EDITABLE_REGIONS } from "./policy.ts";

// Useful to a host that renders paths, and to anyone writing a policy: identity is a segment ARRAY.
export { deepEqual, clone } from "./deep.ts";
export { get, has, pathKey, parsePointer, isPrefix } from "./path.ts";

export type {
  KRMObject,
  Path,
  EventType,
  ErrorCode,
  Projection,
  Scope,
  Identity,
  StreamEvent,
  Change,
  Conflict,
  EditabilityPolicy,
} from "./types.ts";
