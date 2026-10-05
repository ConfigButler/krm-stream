# Native conditional editor

Add this controller when a page watching native resources through a host proxy also needs to edit
them. It composes `connectNativeWatch`, `LiveResourceStore` under the default edit policy and
`nativeObjectURL`. Writes go through the same proxy and collection the watch reads, under the
[editor model](../../docs/client-state-model.md) and the
[native write contract](../../docs/saving.md#native-editing-through-a-host-proxy). It performs no
automatic write retry.

- [editor.ts](editor.ts) captures a save intent synchronously and sends it as a JSON merge PATCH
  with the captured `metadata.uid` and `metadata.resourceVersion` inside it. On 409 it reconciles a
  guarded native GET of the same object. It keeps in-flight edits and never adopts a write response.
- [Native editing tests](../../packages/krm-stream/test/native-editing.test.ts) cover the request
  shape, machinery protection, 409 recovery and conflicts, replaced and deleted objects, refused
  writes, reads overtaken by the watch, and the live-state check.
- `TestRealAPINativeEditThroughHostProxy` (`task test-real-api`) runs this editor and the real
  connector through a credential-holding `/k8s` proxy that validates each PATCH with
  `gateway.ValidateNativeMergePatch`. Against a real API server it checks a save and its echo, a
  competing write between capture and PATCH, the proxy refusing unconditional and machinery
  patches, and an object replaced under the same name.

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
// On unmount:
unsubscribe();
stopConnection();
connection.close();
```

Pass the editor the same `proxy` and `scope` that built the watch URL. It addresses the object under
its own namespace and name, so a collection watched across namespaces works too. Keep one store per
source, scope and login identity. Never point this editor at a projected store, and never give a
projected editor a native response.

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

A successful write's response is the written object. The editor does not read it: the watch delivers
the same object as an ordinary event, and adopting the response could overtake a newer one. A write
that leaves the object unchanged produces no event; if no echo arrives, a guarded read confirms the
state.

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
