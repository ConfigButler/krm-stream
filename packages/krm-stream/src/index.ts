// krm-stream — a live, honest window onto Kubernetes resources, in the browser.
//
// The public surface is small on purpose:
//
//   LiveResourceStore      holds server truth + your draft; three-way merges every watch event;
//                          derives dirtiness; tracks conflicts; builds the merge patch.
//   connectResourceStream  a conforming SSE consumer that feeds a store.
//   resourceStreamURL      builds the stream URL from a scope — the encoding the gateway parses back.
//
// The store is built test-first against conformance/ — the same fixtures the Go gateway runs. See
// ../../../docs/client-state-model.md for the algorithm, ../../../spec/v1.md for the wire, and
// CONTRIBUTING.md for the order of work.
//
// No runtime dependencies, and none of this knows anything about GitOps, Flux, Dex, kcp or
// ConfigButler. It knows KRM.

// Useful to a host that renders paths, and to anyone writing a policy: identity is a segment ARRAY.
export { clone, deepEqual } from "./deep.ts";
export { get, has, isPrefix, parsePointer, pathKey } from "./path.ts";

export { DEFAULT_EDITABLE_REGIONS, defaultPolicy, readOnlyPolicy, regionPolicy } from "./policy.ts";
export type { StreamHandle, StreamOptions } from "./sse.ts";
export { applyStreamEvent, connectResourceStream, connectWithEventSource, SSEDecoder } from "./sse.ts";
export type { ApplyOptions, ApplyResult } from "./store.ts";
export { LiveResourceStore } from "./store.ts";
export type {
  Change,
  Conflict,
  EditabilityPolicy,
  ErrorCode,
  EventType,
  Identity,
  KRMObject,
  Path,
  Projection,
  Scope,
  StreamEvent,
} from "./types.ts";
export type { ScopeQuery } from "./url.ts";
export { resourceStreamURL } from "./url.ts";
export { PROTOCOL_VERSION, VERSION } from "./version.ts";
