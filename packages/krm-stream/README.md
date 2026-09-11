# @configbutler/krm-stream

`@configbutler/krm-stream` is the official dependency-free ESM client for consuming a KRM resource
stream in a browser or JavaScript application. It provides:

- `LiveResourceStore` for server state, local drafts, conflicts, redactions, and merge patches.
- `connectManagedResourceStream` for bounded recovery with cookies or bearer headers.
- `connectWithEventSource` for low-level native EventSource integration.
- `connectResourceStream` for fetch-based transports with explicit headers.
- `resourceStreamURL` for the v1 scope query format.

The package is headless and does not choose a UI framework. It works with the Go gateway in this
repository or any conforming producer.

```ts
import { LiveResourceStore, connectManagedResourceStream, resourceStreamURL } from "@configbutler/krm-stream";

const store = new LiveResourceStore();
connectManagedResourceStream(
  resourceStreamURL("/resource-stream/v1", { version: "v1", resource: "configmaps", namespace: "app" }),
  store,
);
```

## Vendoring it without a bundler

The default entry point is the per-module build: `dist/index.js` imports `./store.js`, which imports
`./merge.js`, and so on. A bundler resolves that graph and tree-shakes it, and a browser resolves it
at runtime, so every file has to be there.

If you have no bundler and no `node_modules` — you copy the library in and serve it yourself — import
the flattened build instead. Same public API, one file, no relative imports left to resolve:

```ts
import { LiveResourceStore } from "@configbutler/krm-stream/bundle";
```

Vendored, that is one file to copy (`dist/krm-stream.js`) and one path to serve:

```js
import { LiveResourceStore, connectManagedResourceStream } from "/krm-stream/krm-stream.js";
```

Both entry points are built from the same source and are exercised by the same browser test suite
against a real `EventSource`. Neither has a runtime dependency.

## Status

Install `@configbutler/krm-stream`. This project is pre-1.0: the protocol and API may still change
before 1.0. Renamed APIs are removed rather than retained as compatibility aliases. See the repository [README](../../README.md),
[client state model](../../docs/client-state-model.md), and [release guide](../../docs/releasing.md).

## Managed connections and conditional saves

```ts
const connection = connectManagedResourceStream(url, store, {
  maxRetries: 8,
  onStateChange: state => renderConnection(state.status),
  // headers: { Authorization: `Bearer ${hostToken}` }, // optional; credentials stay host-owned
});
const unsubscribe = connection.subscribe(state => renderConnection(state.status));
// On teardown:
unsubscribe();
connection.close();
await connection.closed;
```

Import `connectManagedResourceStream` from the package. It uses fetch for same-origin cookies or
bearer headers; `credentials: "include"` opts into cross-origin cookies. It requests a fresh snapshot
on sequence gaps, network failures, HTTP 408/429/5xx and EOF. Existing drafts survive recovery.
States are `connecting`, `syncing`, `live`, `retrying`, `closed`, `terminal`, and `exhausted`.
A reset makes the connection `syncing` until `synced`; enable saves while `live`.

Defaults: eight retries between sustained healthy periods, exponential delay from 500ms capped at 30s,
with jitter. After 30 seconds continuously live, retries and backoff reset (`healthyResetMs` configures
this threshold). Brief snapshots do not replenish the budget; reset, disconnect and cancellation stop
the health timer. Terminal protocol errors and HTTP client
errors (including 401/403, excluding 408/429) stop retries. `close()` or `signal` cancels the stream and
pending backoff; `closed` resolves after cleanup. Create a new handle after credentials change or an
explicit user retry. The low-level fetch and native EventSource connectors remain available; native
EventSource owns network reconnects but closes on sequence gaps. Use the managed connector for
bounded recovery and observable lifecycle state.

Convergence covers projected content excluding `metadata.resourceVersion`, plus redaction records,
once upstream changes quiesce and pending updates arrive. Suppressed final changes can leave the
held RV older: it belongs to the delivered revision and remains a valid conditional-write
precondition, with no freshness or downstream resume guarantee. See the
[contract](../../spec/v1.md#6-ordering-delivery--the-state-guarantee).

`store.captureSave(uid)` captures a detached patch, UID and base resourceVersion together.
`store.captureReconciliation(uid)` guards a projected asynchronous response against newer watch state.
See the [complete conditional-save example](../../examples/conditional-save/README.md) and
[save guide](../../docs/saving.md) for host preconditions, real Kubernetes 409s, and draft preservation.

For Vue 3, use the [copyable composable](../../docs/vue.md) for reactive resource and connection state
with automatic subscription cleanup. Vue stays in the host application.
