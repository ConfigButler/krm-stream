# Native viewer example

A read-only live list of native Kubernetes resources, read through a proxy the host already runs.
One HTML page with no framework, bundler, gateway, editor or write endpoint. It uses
`connectNativeWatch` and `nativeCollectionURL` with `LiveResourceStore(readOnlyPolicy)`, and shows each
resource's identity (namespace, name, kind, UID, resourceVersion), its current fields and the
connection state. **Disconnect**, or leaving the page, closes the connection.

## Run it against a cluster

Build the library, then let `kubectl proxy` act as the host: it serves the API server under `/k8s`
with your kubeconfig credentials and serves this repository's files from the same origin.

```bash
task build-client
kubectl proxy --port=8001 --api-prefix=/k8s/ --www=. --www-prefix=/files/
# http://127.0.0.1:8001/files/examples/native-viewer/?namespace=default
```

Change a ConfigMap in that namespace and the row follows. Query parameters choose the collection:
`proxy` (default `/k8s`), `group`, `version` (default `v1`), `resource` (default `configmaps`),
`namespace` (default `default`; empty for cluster scope), `labelSelector` and `name`. `entry=bundle`
loads the single-file build.

`kubectl proxy` is a development stand-in. A real host serves its own authenticated proxy, such as
krm-foyer's session-authenticated `/k8s`, and decides which collections each user may read. The
browser never holds a Kubernetes credential.

## What it shows and what it does not

- The page lists the collection, applies the complete snapshot and becomes `live` only once the
  watch is accepted. The **History** line shows each connection state.
- Every reconnect lists again; a resource deleted or relabelled out of the selector while
  disconnected disappears when the next snapshot completes.
- Objects are exactly what the proxy returns, Secret values and machinery fields included. Native
  access provides no projection, redaction, suppression or shared watches; use the
  [gateway](../../docs/why-a-gateway.md) for those.
- It is a viewer. To edit through the same proxy, add the
  [native editor](../native-editor/README.md). Resumable watches and paginated lists are later work;
  see the [delivery plan](../../docs/proposals/0006-stream-and-save-implementation-plan.md#open-work-and-delivery-order).

`task e2e-browser` loads this page in Chromium on both entry points, with Playwright standing in for
the host proxy: [native-viewer.spec.ts](../vanilla-browser/tests/native-viewer.spec.ts).
