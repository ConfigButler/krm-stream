# Native conditional editor

Add this controller when a page watching native resources through a host proxy also needs to edit
them. It composes `connectNativeWatch`, `LiveResourceStore` under the default edit policy and
`nativeObjectURL`. Writes go through the same proxy and collection the watch reads, under the
[editor model](../../docs/client-state-model.md) and the
[native write contract](../../docs/saving.md#native-editing-through-a-host-proxy). It performs no
automatic write retry.

- [editor.ts](editor.ts) captures a save intent synchronously and sends it as a JSON merge PATCH
  with the captured `metadata.uid` and `metadata.resourceVersion` inside it. On 409 it reconciles a
  guarded native GET of the same object. After a write whose outcome is unknown, or an accepted
  write whose echo has not arrived, the next Save reads instead of writing again; `confirm()` does
  that read on the host's schedule. It keeps in-flight edits and never adopts a write response.
- [Native editing tests](../../packages/krm-stream/test/native-editing.test.ts) cover the request
  shape, machinery protection, 409 recovery and conflicts, replaced and deleted objects, refused
  writes, unknown write outcomes, confirmation with and without an echo, reads overtaken by the
  watch, and the live-state check.
- `TestRealAPINativeEditThroughHostProxy` (`task test-real-api`) runs this editor and the real
  connector through a credential-holding `/k8s` proxy that validates each PATCH with
  `gateway.ValidateNativeMergePatch`. Against a real API server it checks a save and its echo, a
  competing write between capture and PATCH, the proxy refusing unconditional and machinery
  patches, and an object replaced under the same name.
- [index.html](index.html) is a minimal page that edits one ConfigMap with this editor. Its
  [browser tests](../vanilla-browser/tests/native-editor.spec.ts) are described [below](#run-the-page).

```ts
const proxy = "/k8s";
const scope = { version: "v1", resource: "configmaps", namespace: "app", name: "settings" };
const store = new LiveResourceStore(); // defaultPolicy: spec, labels, annotations, data, stringData
const connection = connectNativeWatch(nativeCollectionURL(proxy, scope), event => applyStreamEvent(store, event));
const stopConnection = connection.subscribe(state => renderConnection(state));
connection.closed.catch(reportApplicationError);
// Once the object is in the store:
const editor = nativeEditor(store, uid, { proxy, scope }, hostFetch, () => connection.state.status === "live");
const unsubscribe = store.subscribe(renderEditor);
// Enable Save only while connection.state.status === "live" and !editor.saving.
// On Save: await editor.save(), then render errors/conflicts and the current draft.
// After "saved" with no echo within the host's patience: await editor.confirm().
// On unmount:
unsubscribe();
stopConnection();
connection.close();
```

Pass the editor the same `proxy` and `scope` that built the watch URL. It addresses the object under
its own namespace and name, so a collection watched across namespaces works too. Keep one store per
source, scope and login identity. Never point this editor at a projected store, and never give a
projected editor a native response.

## Run the page

[index.html](index.html) watches one named ConfigMap and edits its `data` and ordinary annotations,
showing identity, connection state, server state, the draft and pending edits. It has no framework.
The page loads the built library and `editor.ts` compiled into `dist/`, together with the
[keep-local and recovery-copy recipes](../editor-recipes/README.md).
[build.mjs](build.mjs) only strips types and renames the library import to
`@configbutler/krm-stream`. An import map in the page resolves that name to one built entry point.
`dist/` is not committed.

```bash
task build-native-editor   # builds the library, then dist/
kubectl create configmap settings --from-literal=value=hello   # or pick an existing one with name=
kubectl proxy --port=8001 --api-prefix=/k8s/ --www=. --www-prefix=/files/
# http://127.0.0.1:8001/files/examples/native-editor/?namespace=default&name=settings
```

The query parameters are `proxy` (default `/k8s`), `namespace` (default `default`), `name` (default
`settings`) and `echoWaitMs` (default 5000, how long an accepted write waits for its watch echo).
Add `entry=bundle` to load the single-file build.

- Save is enabled only while the watch is `live`, no request is in flight and no conflict is open.
  Typing continues during a save, and inputs keep focus and caret through watch events and reads.
- A dropped or ended watch resumes from the last event applied, without a new snapshot: the draft
  and any conflict stay, Save is disabled until the resumed watch is accepted, and changes made
  meanwhile are merged in as ordinary watch events. After a 410 (expired history) the page lists
  again; Save stays disabled until that snapshot's watch is accepted, and the draft survives it.
- `saved` waits for the watch. The next version it delivers includes the write; fields still dirty
  then were typed after Save or not kept by the server. With no echo within `echoWaitMs`, the page
  offers **Confirm current state**, which calls `editor.confirm()`. A network failure or a 5xx
  offers Confirm at once. Until a read succeeds, Save also only reads, and nothing is resent.
- `confirmed` says the form now builds on the server's state. Fields still dirty were not written.
- Conflicts offer **Use server value** (`store.revert`) and **Keep mine** (the keep-local recipe).
  Typing into a field whose save is in flight becomes a conflict when the echo arrives: the server
  holds the saved value and the draft holds the later typing.
- A refusal (401, 403, 422 or another 4xx) shows the Kubernetes `Status` and keeps the draft.
- Values are edited in text areas, so line breaks survive. A browser text area normalizes `\r\n` to
  `\n`, so editing a value that contains carriage returns writes it back without them.
- A deleted or replaced object shows its unsaved edits from the recovery copy. A replacement opens
  only on request, without the old edits.
- **Disconnect**, or leaving the page, closes the watch, unsubscribes and clears the echo timer. A
  request in flight is not cancelled, because the write may already have landed, and its answer
  changes nothing.

`kubectl proxy` stands in for the host here. It holds your kubeconfig credential server-side, serves
the page and the API from one origin, accepts only `localhost` host names and refuses pod
`exec`/`attach` by default. It provides none of the write protections listed
[under the host proxy](#the-host-proxy). Every request runs as **your** kubeconfig identity, so RBAC
checks you, not the person editing. It does not check the content type, bound the body or run
`ValidateNativeMergePatch`, and it has no CSRF checks, per-user write policy or audit. The page and
store still send only conditional patches without machinery, but nothing stops another client from
sending something else. Keep it on localhost and stop it when you are done.

`task e2e-browser` runs the page in Chromium on both entry points against a test server that serves
the page and plays the host proxy and API server, with a watch that stays open while editing:
[native-editor.spec.ts](../vanilla-browser/tests/native-editor.spec.ts). It covers a conditional
save and its echo, typing while a save is in flight, 409 recovery and a later deliberate Save,
explicit conflict resolution, a lost response and a 502 each settled by a GET before any further
PATCH, an accepted write without an echo confirmed explicitly, refusals, multiline values, a deletion
and a UID replacement each with copy-out, a draft kept through a resumed watch and through the
re-list after an expiry, and disconnect.

## What it protects

- **Machinery.** `metadata.managedFields` and the `kubectl.kubernetes.io/last-applied-configuration`
  annotation are read-only in the store under every policy. They follow the server and never enter
  a patch. Other annotations stay editable one key at a time; the annotation map as a whole cannot be
  replaced or removed, because that would rewrite the last-applied annotation inside it.
- **Concurrency.** The captured `resourceVersion` makes Kubernetes refuse a write based on a version
  the person never saw. The captured `uid` keeps a draft off an object deleted and recreated under the
  same name: Kubernetes checks the version first, so that also answers 409, and the guarded read then
  finds a different UID and reports `unavailable`.
- **Source.** Recovery reads the native object from the same proxy and refuses anything that is not a
  Kubernetes object with this UID, such as a projected endpoint's `{ object, redactedPaths }`.

Native objects carry Secret values. A page that edits a Secret natively shows and sends base64 `data`
values; use a gateway projection when a page must not disclose them.

## Outcomes

`saved`, `unchanged`, `busy`, `draft-conflict`, `version-stale`, `recovering` and `unavailable` mean
what they mean for the [projected conditional editor](../conditional-save/README.md), and the
[saving guide's table](../../docs/saving.md#what-the-person-editing-sees) applies unchanged. A refused
write or read throws `NativeRequestError` with the HTTP status and the Kubernetes `Status`, so a 422
keeps its field causes. A 422 whose cause is `metadata.uid` is an identity precondition, and the
editor recovers from it as from a 409.

`confirmed` is the one outcome the projected editor does not have: a guarded read, not a write,
established the server's current state. Fields still dirty were not written, by this Save or by an
earlier one; the next deliberate Save writes them.

**Never two writes without knowing what the first did.** A network failure or a 5xx (a proxy's 502
included) throws, but the write may have landed, so the next Save performs a guarded read and
returns `confirmed` or another recovery outcome instead of sending the patch again. A 4xx is a
definite refusal and owes no read: correct the draft and save again.

A successful write's response is the written object. The editor does not read it: the watch delivers
the same object as an ordinary event, and adopting the response could overtake a newer one. Until
that echo arrives (the store holds any version after the one the write was based on), the next Save
reads instead of writing. A write that leaves the object unchanged, such as one an admission webhook
reverts, produces no event at all; call `editor.confirm()` when the host stops waiting for an echo.
It reads without writing, and the reverted fields stay dirty.

## The host proxy

The browser writes through the host's proxy, which forwards as the signed-in person; Kubernetes RBAC
still decides what that person may write. Before forwarding a `PATCH`, the proxy should:

- accept only `Content-Type: application/merge-patch+json`. A JSON Patch or an apply patch has other
  semantics;
- bound the body size and run `gateway.ValidateNativeMergePatch` on it. That refuses a patch without
  both preconditions or one touching managed fields or the last-applied annotation;
- decide which resources and fields each user may write, and audit writes, exactly as it decides
  what they may read.

The test proxy in [native_e2e_test.go](../../gateway/kube/native_e2e_test.go) does these checks
before an `httputil.ReverseProxy` forwards the request with the host's credentials.
