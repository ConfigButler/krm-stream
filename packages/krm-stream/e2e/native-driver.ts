// A native viewer and editor on stdin/stdout, for the Go real-API suite
// (gateway/kube/native_e2e_test.go).
//
// The Go test owns the cluster and a host-style /k8s proxy that holds the credentials. This process is
// the browser: the REAL connectNativeWatch feeding the REAL store under the default edit policy, and
// the REAL native editor from examples/native-editor writing back through the same proxy, with no
// gateway in between. After every state event and connection state it prints one JSON line
// describing what the store holds, so the Go side can wait for the cluster's changes to arrive.
//
// Commands, one per line on stdin:
//
//	close                                          close the connection; the driver exits once `closed` settles
//	{"op":"set","name":…,"path":[…],"value":…}     edit the named member's draft
//	{"op":"save","name":…}                         save it; prints {"saved":<outcome>}
//
// A command that fails prints {"commandError":…}.
//
//	node e2e/native-driver.ts <proxy base> <scope JSON>

import { createInterface } from "node:readline";
import { nativeEditor } from "../../../examples/native-editor/editor.ts";
import {
  applyStreamEvent,
  connectNativeWatch,
  LiveResourceStore,
  type NativeScope,
  nativeCollectionURL,
  type Path,
} from "../src/index.ts";

const [proxy, scopeJSON] = process.argv.slice(2);
if (proxy === undefined || scopeJSON === undefined)
  throw new Error("usage: native-driver.ts <proxy base> <scope JSON>");
const scope = JSON.parse(scopeJSON) as NativeScope;

const store = new LiveResourceStore();
const report = (line: object) => process.stdout.write(`${JSON.stringify(line)}\n`);
const members = () =>
  store
    .ids()
    .map((uid) => {
      const object = store.server(uid);
      return {
        uid,
        name: object.metadata.name,
        kind: object.kind,
        resourceVersion: object.metadata.resourceVersion,
        data: object.data,
      };
    })
    .sort((a, b) => a.name.localeCompare(b.name) || a.uid.localeCompare(b.uid));

const handle = connectNativeWatch(
  nativeCollectionURL(proxy, scope),
  (event) => {
    applyStreamEvent(store, event);
    report({ event: event.type, members: members() });
  },
  { retryDelayMs: 100, onError: (code, message, terminal) => report({ error: { code, message, terminal } }) },
);
let live = false;
handle.subscribe((state) => {
  live = state.status === "live";
  report({ state: state.status });
});

// One editor per UID, so a guarded read it still owes survives between saves.
const editors = new Map<string, ReturnType<typeof nativeEditor>>();
const uidOf = (name: string) => {
  const uid = store.ids().find((id) => store.server(id).metadata.name === name);
  if (uid === undefined) throw new Error(`${name} is not in the store`);
  return uid;
};

type Command = { op: "set"; name: string; path: Path; value: unknown } | { op: "save"; name: string };

async function run(command: Command) {
  const uid = uidOf(command.name);
  if (command.op === "set") return store.setValue(uid, command.path, command.value);
  let editor = editors.get(uid);
  if (!editor) {
    editor = nativeEditor(store, uid, { proxy: proxy!, scope }, fetch, () => live);
    editors.set(uid, editor);
  }
  report({ saved: await editor.save() });
}

const input = createInterface({ input: process.stdin });
input.on("line", (line) => {
  if (line.trim() === "close") return handle.close();
  run(JSON.parse(line) as Command).catch((error: unknown) => report({ commandError: String(error) }));
});
input.on("close", () => handle.close());
await handle.closed.finally(() => input.close());
