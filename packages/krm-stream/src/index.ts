// krm-stream — a live, honest window onto Kubernetes resources, in the browser.
//
// The public surface is small on purpose:
//
//   LiveResourceStore      holds server truth + your draft; three-way merges every watch event;
//                          derives dirtiness; tracks conflicts; builds the merge patch.
//   connectResourceStream  a conforming SSE consumer that feeds a store.
//
// Neither exists yet. This package currently ships the protocol TYPES — the half of the contract
// that both implementations already agree on — while the store is built test-first against
// conformance/. See ../../../docs/client-state-model.md for the algorithm it must implement, and
// CONTRIBUTING.md for the order of work.

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
