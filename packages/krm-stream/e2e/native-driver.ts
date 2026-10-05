// A native viewer on stdout, for the Go real-API suite (gateway/kube/native_e2e_test.go).
//
// The Go test owns the cluster and a host-style /k8s proxy that holds the credentials. This process is
// the browser: the REAL connectNativeWatch feeding the REAL read-only store, with no gateway in
// between. After every state event and connection state it prints one JSON line describing what the
// store holds, so the Go side can wait for the cluster's changes to arrive. A line `close` on stdin
// closes the connection; the driver exits once `closed` settles.
//
//	node e2e/native-driver.ts <collection URL>

import { createInterface } from "node:readline";
import { applyStreamEvent, connectNativeWatch, LiveResourceStore, readOnlyPolicy } from "../src/index.ts";

const url = process.argv[2];
if (!url) throw new Error("usage: native-driver.ts <collection URL>");

const store = new LiveResourceStore(readOnlyPolicy);
const report = (line: object) => process.stdout.write(`${JSON.stringify(line)}\n`);
const members = () =>
  store
    .ids()
    .map((uid) => {
      const object = store.server(uid);
      return { uid, name: object.metadata.name, kind: object.kind, data: object.data };
    })
    .sort((a, b) => a.name.localeCompare(b.name) || a.uid.localeCompare(b.uid));

const handle = connectNativeWatch(
  url,
  (event) => {
    applyStreamEvent(store, event);
    report({ event: event.type, members: members() });
  },
  { retryDelayMs: 100, onError: (code, message, terminal) => report({ error: { code, message, terminal } }) },
);
handle.subscribe((state) => report({ state: state.status }));

const input = createInterface({ input: process.stdin });
input.on("line", (line) => {
  if (line.trim() === "close") handle.close();
});
input.on("close", () => handle.close());
await handle.closed.finally(() => input.close());
