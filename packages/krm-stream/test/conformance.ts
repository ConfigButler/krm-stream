// The client's half of the conformance loader. It reads the SAME generated JSON the Go suite reads
// (conformance/gen/) — that shared read is the entire reason this is one repository.
//
// Keep this file's semantics identical to gateway/conformance.go. If the two loaders drift, the
// "contract" is just two implementations agreeing to disagree.

import { readFileSync } from "node:fs";
import type {
  ErrorCode,
  EventType,
  Identity,
  KRMObject,
  Path,
  Projection,
  Redaction,
  Scope,
  StreamEvent,
} from "../src/types.ts";

// Resolved relative to THIS file, not to the working directory — so `node --test` works from
// anywhere, and so does an editor's inline test runner.
const CONFORMANCE = new URL("../../../conformance/", import.meta.url);

/** An edit the fixture applies partway through the stream. `after: N` means "once event index N has
 * been processed" — so an edit can be placed BEFORE the server change that will collide with it.
 * A fixture format that could not express that ordering could not test a three-way merge at all. */
export interface FixtureEdit {
  after: number;
  op: "set" | "remove" | "addKey" | "renameKey" | "revert" | "adopt";
  uid: string;
  path: Path;
  value?: unknown;
  newKey?: string;
  /** `adopt` only: the bodies/ reference to hand `store.adoptSaved`. An adopt is not an edit to a
   * delivered object — it IS a delivery, of the object a save returned — so it carries a `body`, not a
   * `uid`/`path` like the other ops. */
  body?: string;
}

export interface FixtureExpect {
  uids?: string[];
  dirty?: Path[];
  conflicts?: (Path | { path: Path; theirs?: unknown })[];
  draftSubset?: Record<string, unknown>;
  absentPaths?: Path[];
  readOnlyPaths?: Path[];
  flashed?: Path[];
  patch?: Record<string, unknown> | null;
}

export interface FixtureCheckpoint extends FixtureExpect {
  after: number;
}

export interface FixtureEvent {
  type: EventType;
  body?: string;
  redacted?: Redaction[];
  identity?: Identity;
  code?: ErrorCode;
  terminal?: boolean;
}

export interface Fixture {
  id: string;
  title: string;
  why: string;
  suites?: string[];
  scope?: Scope;
  projection?: Projection;
  watch?: unknown[];
  events: FixtureEvent[];
  client?: { edits?: FixtureEdit[]; expect?: FixtureExpect };
  checkpoints?: FixtureCheckpoint[];
}

const bodies: Record<string, KRMObject> = JSON.parse(readFileSync(new URL("gen/bodies.json", CONFORMANCE), "utf8"));
const fixtures: Fixture[] = JSON.parse(readFileSync(new URL("gen/fixtures.json", CONFORMANCE), "utf8"));

/** A KRM object by its bodies/ reference. Missing is a hard error: a fixture naming an object that
 * does not exist is a broken contract, not a skippable test. */
export function body(ref: string): KRMObject {
  const obj = bodies[ref];
  if (!obj) throw new Error(`conformance: no such body ${JSON.stringify(ref)}`);
  return structuredClone(obj);
}

/** Every fixture this suite must run. A fixture with no `suites` is the client's by default. */
export function clientFixtures(): Fixture[] {
  return fixtures.filter((f) => (f.suites ?? ["client"]).includes("client"));
}

export function allFixtures(): Fixture[] {
  return fixtures;
}

export function allBodies(): Record<string, KRMObject> {
  return bodies;
}

/** Turn a fixture event into the StreamEvent that must actually appear on the wire — i.e. what the
 * gateway would have sent, and therefore exactly what the store gets fed. */
export function resolve(f: Fixture, fe: FixtureEvent): StreamEvent {
  switch (fe.type) {
    case "reset":
      return { seq: 0, type: "reset", target: f.scope?.target, scope: f.scope, projection: f.projection };
    case "added":
    case "modified":
      return { seq: 0, type: fe.type, object: body(fe.body!), redacted: fe.redacted ?? [] };
    case "deleted":
      return { seq: 0, type: "deleted", identity: fe.identity };
    case "synced":
      return { seq: 0, type: "synced" };
    case "error":
      return { seq: 0, type: "error", code: fe.code, terminal: fe.terminal ?? false };
    default:
      throw new Error(`conformance: unknown event type ${JSON.stringify(fe.type)}`);
  }
}
