// The end of the ladder that needs no cluster: a REAL Go gateway, a REAL socket, a REAL fetch.
//
// Not part of `node --test` — it needs the replay server running, so it is its own thing:
//
//	task e2e-wire
//
// Everything below it in the ladder feeds the store from bytes that never left the process. That
// proves the protocol, and it proves the parser, and it cannot prove any of this:
//
//   - the response really is `text/event-stream`, and no proxy-defeating header is missing;
//   - the server really FLUSHES each frame instead of buffering until the socket fills, which is the
//     difference between a live status watch and a batch job;
//   - the frames really do arrive split across arbitrary chunk boundaries;
//   - v1's "no SSE `id:` lines, ever" really holds on the bytes, not just in a unit test;
//   - a terminal error really CLOSES the connection.
//
// All five are invisible in-process, and every one of them breaks a browser.

import assert from "node:assert/strict";
import { connectResourceStream, LiveResourceStore } from "../src/index.ts";
import type { Path } from "../src/types.ts";
import { clientFixtures } from "../test/conformance.ts";
import { applyEdit, check } from "../test/expect.ts";

const BASE = process.env.REPLAY_URL ?? "http://127.0.0.1:8080";

let failures = 0;
const ok = (name: string, fn: () => Promise<void> | void) =>
  Promise.resolve()
    .then(fn)
    .then(() => console.log(`  ok  ${name}`))
    .catch((err: Error) => {
      failures++;
      console.error(`  NOT OK  ${name}\n    ${err.message.split("\n").join("\n    ")}`);
    });

const url = (id: string, conn = 0) => `${BASE}/resource-stream/v1?fixture=${id}&connection=${conn}`;

/** The fixtures with both a watch script (so the server can serve them) and client expectations. */
const fixtures = clientFixtures().filter((f) => (f.suites ?? []).includes("gateway") && (f.watch?.length ?? 0) > 0);

console.log(`e2e: driving ${fixtures.length} fixtures against a real gateway at ${BASE}\n`);

await ok("the server is up", async () => {
  const res = await fetch(`${BASE}/healthz`);
  assert.equal(res.status, 200, "start it with `task e2e-wire`, or REPLAY_URL=… to point elsewhere");
});

await ok("the response is a conforming SSE stream, and says so", async () => {
  const res = await fetch(url("snapshot-then-deltas"));
  assert.equal(res.headers.get("content-type"), "text/event-stream");
  assert.equal(res.headers.get("x-krm-stream-protocol"), "1");
  // A stream a proxy may buffer is not a stream. This header is why nginx does not sit on the
  // response until it is "big enough" — which turns a live watch into a batch job nobody can debug.
  assert.equal(res.headers.get("x-accel-buffering"), "no");
  assert.match(res.headers.get("cache-control") ?? "", /no-cache/);
  await res.body?.cancel();
});

await ok("v1 emits no SSE id: lines, on the actual bytes", async () => {
  // Putting a resource uid in `id:` is the tempting thing, and it silently gives the browser's
  // automatic Last-Event-ID reconnect an entirely incorrect meaning. Assert on the wire, not on a
  // struct: the struct cannot lie about a field it does not have, but a hand-written writer can.
  const text = await drain(url("resync-midstream"));
  for (const line of text.split("\n")) {
    assert.ok(!line.startsWith("id:"), `an SSE id: line reached the wire: ${line}`);
  }
});

for (const f of fixtures) {
  await ok(`${f.id}: over HTTP, the store ends up holding what the fixture says`, async () => {
    const store = new LiveResourceStore();
    const flashed: Path[] = [];

    // The index a fixture's `after:` refers to: position in the fixture's `events:` list. Count it
    // from the STREAM callbacks — one per delivered event, errors included — and not from the store's
    // `subscribe`, which also fires on our own edits and would drift by one the moment a fixture
    // edited anything. (It did. That is why this comment exists.)
    let i = -1;
    const onEvent = () => {
      i++;
      for (const edit of f.client?.edits ?? []) {
        // Applied mid-stream, while the bytes are still arriving. That ordering IS the three-way
        // merge, and it is the one thing a "fetch it all, then assert" e2e cannot express.
        if (edit.after === i) applyEdit(store, edit);
      }
    };

    const connections = (f.watch ?? []).filter((op) => (op as { op: string }).op === "disconnect").length + 1;
    for (let conn = 0; conn < connections; conn++) {
      const handle = connectResourceStream(url(f.id, conn), store, {
        onChange: (paths) => {
          flashed.push(...paths);
          onEvent();
        },
        onError: () => onEvent(), // an `error` event occupies an index too (see resync-midstream)
      });
      await handle.closed;
    }

    if (f.client?.expect) check(store, flashed, f.client.expect, `${f.id} (over HTTP)`);
  });
}

console.log(failures === 0 ? "\ne2e: all green" : `\ne2e: ${failures} FAILED`);
process.exit(failures === 0 ? 0 : 1);

/** Read a stream to the end and return its raw bytes — for the assertions that are about the WIRE
 * rather than about the state it produces. */
async function drain(u: string): Promise<string> {
  const res = await fetch(u);
  const reader = res.body!.pipeThrough(new TextDecoderStream()).getReader();
  let text = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) return text;
    text += value;
  }
}
