// The seam. This is the test that did not exist, and the reason "end to end" was a claim rather than
// a fact.
//
// Until now the two implementations shared a DOCUMENT: the Go suite asserted "given this watch I
// would emit these events", the TypeScript suite asserted "given these events I hold this state", and
// nothing anywhere checked that the events one side emits are bytes the other side can read. Two
// implementations agreeing to disagree is precisely what that gap permits.
//
// So this suite reads conformance/gen/sse/<id>.sse — the bytes the Go gateway REALLY WROTE, through
// its real SSE sink, committed and staleness-checked — parses them with the real SSEDecoder, replays
// them into a real store, applies the fixture's edits at their `after:` positions, and asserts the
// same `expect:` that store.test.ts asserts against hand-built objects.
//
// If the gateway ever changes what it puts on the wire, this fails. If the client ever stops being
// able to read it, this fails. That is the whole job.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  applyStreamEvent,
  connectResourceStream,
  LiveResourceStore,
  SSEDecoder,
  StreamSequence,
} from "../src/index.ts";
import type { Path, StreamEvent } from "../src/types.ts";
import { clientFixtures, resolve } from "./conformance.ts";
import { applyEdit, check } from "./expect.ts";

const SSE = new URL("../../../conformance/gen/sse/", import.meta.url);

/** The fixtures that exist on BOTH sides — the ones with a `watch:` script (so the gateway can
 * produce a transcript) and a client `expect:` (so there is something to hold it to). */
function wireFixtures() {
  return clientFixtures().filter((f) => (f.suites ?? []).includes("gateway") && (f.watch?.length ?? 0) > 0);
}

for (const f of wireFixtures()) {
  test(`${f.id}: the client reads what the gateway wrote`, () => {
    const bytes = readFileSync(new URL(`${f.id}.sse`, SSE), "utf8");

    // Chunked at a hostile boundary: ONE BYTE AT A TIME. A frame split down the middle is not a
    // hypothetical — it is what a real network does, under exactly the load where you least want to
    // be debugging it — and a decoder that only works on whole frames passes every naive test and
    // then corrupts the first busy stream it meets.
    const events = decodeInChunks(bytes, 1);

    const store = new LiveResourceStore();
    const flashed: Path[] = [];
    for (const [i, ev] of events.entries()) {
      flashed.push(...applyStreamEvent(store, ev));
      for (const edit of f.client?.edits ?? []) {
        if (edit.after === i) applyEdit(store, edit);
      }
      for (const cp of f.checkpoints ?? []) {
        if (cp.after === i) check(store, flashed, cp, `${f.id} @checkpoint after=${i}`);
      }
    }

    if (f.client?.expect) check(store, flashed, f.client.expect, `${f.id} (from the wire)`);
  });
}

test("the transcript on the wire IS the fixture's events — same order, same count", () => {
  // The `after:` indices in a fixture address its `events:` list. If the gateway emitted a different
  // number of events, every edit in every fixture would be applied at the wrong moment and the suite
  // above would be quietly testing something else. The Go side asserts this too, byte for byte; this
  // is the assertion from the other end of the wire, which is the only end that matters to a browser.
  for (const f of wireFixtures()) {
    const events = decodeInChunks(readFileSync(new URL(`${f.id}.sse`, SSE), "utf8"), 64);
    assert.deepEqual(
      events.map((e) => e.type),
      f.events.map((e) => e.type),
      `${f.id}: the wire and the fixture disagree about what was sent`,
    );
    for (const [i, ev] of events.entries()) {
      assert.deepEqual(
        wireForm(ev),
        wireForm(resolve(f, f.events[i]!)),
        `${f.id} event ${i}: the wire is not the fixture`,
      );
      if (ev.type === "error") {
        // `terminal` must be PRESENT, not merely falsy. A consumer reading "do I give up, or do I
        // retry?" out of a field that is not there is how a browser ends up reconnecting to a
        // forbidden scope forever — EventSource retries on its own unless it is told not to.
        assert.ok("terminal" in ev, `${f.id} event ${i}: an error must say whether it is terminal`);
        assert.ok(ev.message, `${f.id} event ${i}: an error with no message is a diagnostic that says nothing`);
      }
    }
  }
});

/** The event as the CORPUS specifies it — which is everything except an error's `message`.
 *
 * That one field is compared loosely, and not for convenience. `code` is the normative part: it is a
 * stable, machine-readable set precisely because "a free-form message alone is not enough for stable
 * client behavior" (spec §4.3), and the fixture format carries no `message` field at all, in either
 * loader. Pinning the prose would assert a rule the contract does not make — and would push a gateway
 * toward emitting nothing, which is worse for whoever debugs it at 3am. The Go suite draws the line
 * in exactly the same place; if it ever moves, it moves on both sides. */
function wireForm(ev: StreamEvent): StreamEvent {
  const { message: _message, seq: _seq, ...rest } = ev;
  return { ...rest, seq: 0 } as StreamEvent;
}

test("a heartbeat is not an event, and neither is any other comment", () => {
  // Spec §7: heartbeats are SSE comments, and a consumer ignores them. Get this wrong and every 20
  // seconds the store is handed a null event — or, worse, the parser throws and a live status watch
  // dies quietly on an idle connection.
  const d = new SSEDecoder();
  const events = d.push(
    ": heartbeat\n\n" +
      ": connection 1\n\n" +
      '\ndata: {"type":"reset"}\n\n' +
      ": heartbeat\n\n" +
      'data: {"type":"synced"}\n\n',
  );
  assert.deepEqual(
    events.map((e) => e.type),
    ["reset", "synced"],
  );
});

test("a sequence gap is detected before the later event is applied", () => {
  const sequence = new StreamSequence();
  assert.equal(sequence.observe({ seq: 1, type: "reset" }), null);
  assert.deepEqual(sequence.observe({ seq: 3, type: "synced" }), { expected: 2, received: 3 });
  assert.deepEqual(sequence.observe({ seq: Number.NaN, type: "synced" }), { expected: 2, received: Number.NaN });
});

test("an unknown event type and an unknown field are ignored, not fatal", () => {
  // Spec §0: a minor addition to v1 — a new optional event type, a new optional envelope field —
  // must not break an older client. This is the rule that lets the protocol grow at all.
  const store = new LiveResourceStore();
  const events = new SSEDecoder().push(
    'data: {"type":"reset","somethingNew":true}\n\n' +
      'data: {"type":"future-event","payload":42}\n\n' +
      'data: {"type":"synced"}\n\n',
  );
  assert.equal(events.length, 3);
  for (const ev of events) applyStreamEvent(store, ev);
  assert.deepEqual(store.ids(), [], "the store survived an event type it has never heard of");
});

test("a truncated stream yields no half-parsed event", () => {
  // The connection died mid-frame. The right answer is silence — NOT a partial object, which the
  // store would apply as a REPLACEMENT and blank the resource out on screen.
  const d = new SSEDecoder();
  assert.deepEqual(d.push('data: {"type":"added","object":{"apiV'), []);
  assert.deepEqual(d.push(""), []);
});

test("a CRLF delimiter split across chunks is one newline, not two", () => {
  const d = new SSEDecoder();
  assert.deepEqual(d.push('data: {"seq":1,"type":"reset"}\r'), []);
  assert.deepEqual(d.push("\n\r"), []);
  assert.deepEqual(d.push("\n"), [{ seq: 1, type: "reset" }]);
});

test("an already-aborted signal never opens a fetch stream", async () => {
  const signal = new AbortController();
  signal.abort();
  let calls = 0;
  const handle = connectResourceStream("https://example.invalid/stream", new LiveResourceStore(), {
    signal: signal.signal,
    fetch: async () => {
      calls++;
      return new Response();
    },
  });
  await handle.closed;
  assert.equal(calls, 0);
});

/** Decode the whole transcript, feeding it `size` characters at a time. */
function decodeInChunks(bytes: string, size: number): StreamEvent[] {
  const d = new SSEDecoder();
  const out: StreamEvent[] = [];
  for (let i = 0; i < bytes.length; i += size) {
    out.push(...d.push(bytes.slice(i, i + size)));
  }
  return out;
}
