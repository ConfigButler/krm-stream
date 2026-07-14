// The wire vocabulary and the client's core types. This file is the TypeScript half of the same
// contract gateway/event.go declares in Go; see ../../../spec/v1.md, which governs both.
//
// Nothing here has a runtime dependency, and nothing here knows about ConfigButler, GitOps, Dex or
// kcp. It knows KRM. That one-way rule is what makes this library adoptable by someone who has
// never heard of us.

/** The complete projected Kubernetes object, verbatim.
 *
 * Deliberately NOT a schema: a ConfigMap has `data` and no `spec`; a Secret has `type` and
 * `stringData`; a CRD may define any field at the root. We carry whatever is there. */
export interface KRMObject {
  apiVersion: string;
  kind: string;
  metadata: {
    uid: string;
    name: string;
    namespace?: string;
    resourceVersion?: string;
    labels?: Record<string, string>;
    annotations?: Record<string, string>;
    [key: string]: unknown;
  };
  [key: string]: unknown;
}

/** A field address: an ARRAY of segments — object keys (any string) and array indices.
 *
 * Never a dot-joined string. `metadata.labels["app.kubernetes.io/name"]` is THREE segments, the last
 * of which contains two dots and a slash; joining it produces an address that resolves to nothing,
 * silently. That bug (R-ID) is why this type exists. */
export type Path = (string | number)[];

export type EventType = "reset" | "added" | "modified" | "deleted" | "synced" | "error";

export type ErrorCode =
  | "FORBIDDEN"
  | "UNAUTHENTICATED"
  | "SCOPE_INVALID"
  | "UPSTREAM_UNAVAILABLE"
  | "RESYNC_REQUIRED"
  | "SLOW_CONSUMER"
  | "INTERNAL";

/** What the gateway removed from every object in this stream. On the wire, per cycle, because a
 * consumer must be able to distinguish "absent upstream" from "the gateway took it away". */
export type Projection = "krm-raw/v1" | "krm-full/v1" | "krm-spec/v1";

/** A path whose value exists upstream but was withheld by the projection. `rev` starts at one and
 * increments whenever that hidden value changes during this stream connection. */
export interface Redaction {
  path: string;
  rev: number;
}

export interface Scope {
  target: string;
  group?: string;
  version: string;
  resource: string;
  namespace?: string;
  name?: string;
  labelSelector?: string;
}

/** The tombstone. `deleted` is the one event with no complete object — there may not be one — so it
 * carries an identity instead. `uid` is what the consumer acts on. */
export interface Identity {
  uid: string;
  apiVersion: string;
  kind: string;
  namespace?: string;
  name: string;
}

export interface StreamEvent {
  /** Per-connection event sequence. A gap means the consumer must reconnect for a fresh snapshot. */
  seq: number;
  type: EventType;
  // reset
  target?: string;
  scope?: Scope;
  projection?: Projection;
  // added / modified
  object?: KRMObject;
  /** Always present on added/modified — empty when nothing is redacted. */
  redacted?: Redaction[];
  // deleted
  identity?: Identity;
  // error
  code?: ErrorCode;
  message?: string;
  terminal?: boolean;
  retryAfterMs?: number | null;
}

/** One pending edit, derived by comparing the draft to the last server object. Never cached — a
 * cached dirty set goes stale on the next watch event, which is regression R-DERIVED. */
export interface Change {
  path: Path;
  kind: "add" | "update" | "delete";
  old: unknown;
  new: unknown;
}

/** "The server moved this field while you were editing it, and to a different value than you
 * typed." `theirs` is what the cluster says; the user's edit is never silently overwritten. */
export interface Conflict {
  path: Path;
  theirs: unknown;
}

/** Which regions of an object a user may edit. `status` is controller-owned and read-only — but
 * read-only is NOT ignored: it still follows the server live and flashes what changed. That is the
 * entire live-status-watch use case. */
export interface EditabilityPolicy {
  isEditable(obj: KRMObject, path: Path): boolean;
  /** True for a path that is not editable itself but that an editable region lives UNDER — `[]` and
   * `["metadata"]` under the default policy.
   *
   * The merge needs both questions answered, and they are not each other's negation. `metadata` is
   * not editable, but replacing it wholesale from the server would clobber the label the user is
   * editing inside it; `status` is not editable either, and replacing it wholesale is exactly right.
   * Without this second predicate a store cannot tell those two apart. */
  containsEditable(obj: KRMObject, path: Path): boolean;
  /** For an associative Kubernetes list at path, return its identity fields. Omit this to retain the
   * safe atomic-array behavior. Implementations normally derive it from OpenAPI's
   * `x-kubernetes-list-type: map` and `x-kubernetes-list-map-keys`. */
  listMapKeys?(obj: KRMObject, path: Path): readonly string[] | undefined;
}
