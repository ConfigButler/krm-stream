// A browser editor on stdin/stdout, for the Go real-API suite (gateway/kube/composition_e2e_test.go).
//
// The Go test owns the cluster, the gateway and the host's save endpoint. This process owns the REAL
// store and the REAL conditional editor from examples/conditional-save, so what is asserted is the
// composition a browser runs, not a Go imitation of it. The Go side decides exactly when each stream
// event reaches the store; that is how a test lands a reconciliation read between a snapshot's
// `reset` and its `synced`.
//
// One JSON command per line in, one JSON reply per line out, matched by `id`. A save replies when it
// settles and does not hold up the commands behind it, so events can arrive while it is in flight.

import { createInterface } from "node:readline";
import { conditionalEditor } from "../../../examples/conditional-save/editor.ts";
import { applyStreamEvent, LiveResourceStore, type Path } from "../src/index.ts";
import { toStateEvent } from "../src/sse.ts";
import type { StreamEvent } from "../src/types.ts";

type Command = { id: number } & (
  | { op: "event"; event: StreamEvent }
  | { op: "set"; uid: string; path: Path; value: unknown }
  | { op: "save"; uid: string; url: string }
  | { op: "state"; uid: string }
  | { op: "requests" }
);

interface HostRequest {
  method: string;
  status: number;
  body: unknown;
}

const store = new LiveResourceStore();
// Live as the connector would publish it: after a snapshot completes, until the next one begins.
let live = false;
const editors = new Map<string, ReturnType<typeof conditionalEditor>>();
let requests: HostRequest[] = [];

const host: typeof fetch = async (input, init) => {
  const response = await fetch(input, init);
  const text = await response.clone().text();
  let body: unknown = text;
  try {
    body = JSON.parse(text);
  } catch {
    // not JSON: keep the text
  }
  requests.push({ method: init?.method ?? "GET", status: response.status, body });
  return response;
};

async function run(command: Command): Promise<unknown> {
  switch (command.op) {
    case "event": {
      const event = toStateEvent(command.event);
      if (!event) return { ignored: command.event.type };
      applyStreamEvent(store, event);
      if (event.type === "reset") live = false;
      if (event.type === "synced") live = true;
      return { applied: event.type };
    }
    case "set":
      store.setValue(command.uid, command.path, command.value);
      return {};
    case "save": {
      const key = `${command.uid} ${command.url}`;
      let editor = editors.get(key);
      if (!editor) {
        editor = conditionalEditor(store, command.uid, command.url, host, () => live);
        editors.set(key, editor);
      }
      return { outcome: await editor.save() };
    }
    case "state": {
      const uid = command.uid;
      if (!store.ids().includes(uid)) return { present: false, live };
      return {
        present: true,
        live,
        draft: store.draft(uid),
        server: store.server(uid),
        changes: store.changes(uid),
        conflicts: store.conflicts(uid),
      };
    }
    case "requests": {
      const out = requests;
      requests = [];
      return { requests: out };
    }
  }
}

for await (const line of createInterface({ input: process.stdin })) {
  const command = JSON.parse(line) as Command;
  void run(command).then(
    (reply) => process.stdout.write(`${JSON.stringify({ id: command.id, ...(reply as object) })}\n`),
    (error: unknown) => process.stdout.write(`${JSON.stringify({ id: command.id, error: String(error) })}\n`),
  );
}
