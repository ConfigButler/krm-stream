# @configbutler/krm-stream

`@configbutler/krm-stream` is the official dependency-free ESM client for consuming a KRM resource
stream in a browser or JavaScript application. It provides:

- `connectResourceStream`, the connector: it reads the stream over fetch, with session cookies or
  bearer headers, and hands each resource state event to a callback, with bounded recovery.
- `LiveResourceStore` for server state, local drafts, conflicts, redactions, and merge patches, and
  `applyStreamEvent` to apply a state event to it.
- `resourceStreamURL` for the v1 scope query format.

Start with a watch-backed list or viewer; add draft reconciliation only when the page needs editing.
The package is headless and does not choose a UI framework. Today it consumes gateway SSE through
fetch, from the Go gateway or another v1 producer. A fetch-based native Kubernetes watch connector
is the [preferred direction and requested work](../../docs/field-reports/third-our-identity.md#native-watch-connector),
with gateway SSE retained as a compatibility path. It is not an available connector yet.

```ts
import { LiveResourceStore, readOnlyPolicy, applyStreamEvent, connectResourceStream, resourceStreamURL } from "@configbutler/krm-stream";

const store = new LiveResourceStore(readOnlyPolicy);
const connection = connectResourceStream(
  resourceStreamURL("/resource-stream/v1", { version: "v1", resource: "configmaps", namespace: "app" }),
  (event) => applyStreamEvent(store, event),
);
connection.closed.catch(reportApplicationError);
// Close connection and dispose rendering subscriptions on view teardown.
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
import { LiveResourceStore, applyStreamEvent, connectResourceStream } from "/krm-stream/krm-stream.js";
```

Both entry points are built from the same source and are exercised by the same browser test suite in
Chromium. Neither has a runtime dependency.

## Status

Install `@configbutler/krm-stream`. This project is pre-1.0: the protocol and API may still change
before 1.0. Renamed APIs are removed rather than retained as compatibility aliases; see
[upgrading from 0.7](../../docs/migrating.md). See the repository [README](../../README.md),
[client state model](../../docs/client-state-model.md), and [release guide](../../docs/releasing.md).

## Connections

```ts
const connection = connectResourceStream(url, event => renderChange(applyStreamEvent(store, event)), {
  maxRetries: 8,
  onError: (code, message, terminal) => showStreamError(code, message, terminal),
  // headers: { Authorization: `Bearer ${hostToken}` }, // optional; credentials stay host-owned
});
renderConnection(connection.state);
const unsubscribe = connection.subscribe(state => renderConnection(state));
connection.closed.catch(reportApplicationError);
// On teardown:
unsubscribe();
connection.close();
```

`connectResourceStream` is the package's one connector. It uses fetch for same-origin cookies or
bearer headers; `credentials: "include"` opts into cross-origin cookies. It decodes and
sequence-checks the stream, and calls your callback synchronously, exactly once per state event
(`reset`, `added`, `modified`, `deleted`, `synced`), in stream order, without the wire `seq`.
The callback must finish applying each event before it returns, so do not pass an `async` function:
it type-checks, but `live` could then be published before `synced` is applied, and its exceptions
would never reach `closed`. Errors go to `onError`, never to the callback; unknown event types are
ignored. It requests a fresh
snapshot on sequence gaps, network failures, HTTP 408/429/5xx, EOF, and a connection the gateway
closes after a retryable error such as `UPSTREAM_UNAVAILABLE`. Drafts for surviving UIDs survive
recovery; deletion and completed snapshot pruning remove them.

Read `connection.state` for the initial presentation and `subscribe` for every later state. States are
`connecting`, `syncing`, `live`, `retrying`, `closed`, `terminal`, and `exhausted`. A reset makes the
connection `syncing` before your callback sees it, and `live` is published after your callback has
applied `synced`; enable saves while `live`. A `retrying` state caused by a sequence gap carries
`gap: { expected, received }`; the next attempt clears it.

Defaults: eight retries between sustained healthy periods, exponential delay from 500ms capped at 30s,
with jitter. After 30 seconds continuously live, retries and backoff reset (`healthyResetMs` configures
this threshold). Brief snapshots do not replenish the budget; reset, disconnect and cancellation stop
the health timer. Terminal protocol errors and HTTP client
errors (including 401/403, excluding 408/429) stop retries. `close()` or `signal` cancels the stream and
pending backoff; `closed` resolves after cleanup. Create a new handle after credentials change or an
explicit user retry.

An exception from your own code is a bug, not a network failure, so it is never retried. If the
callback, a `subscribe` callback or `onError` throws, even after calling `close()`, the stream stops,
every subscriber still receives the state being published, the final state is `closed`, and `closed`
rejects with the first exception once the reader, timers and listeners are released. Always attach
a rejection handler (`connection.closed.catch(reportApplicationError)`) or await `closed`; otherwise
such a bug surfaces as an unhandled rejection. A gateway's `X-KRM-Stream-Protocol` header is optional;
a stream that names a different protocol version is refused as a terminal `INTERNAL` error before any
event is applied.

The server's retry hint sets the least the next reconnect waits, within `maxRetryDelayMs`: an HTTP
`Retry-After`, or an error event's `retryAfterMs`. `onError` receives it as its fourth argument. When
a refusal's body is a Kubernetes `Status`, as a host proxying `/k8s` might send, `onError` receives
its `message` instead of `stream: HTTP 403`.

There is no official `EventSource` helper. It cannot send explicit authorization headers and leaves
reconnect timing to the browser; a host-owned helper must handle protocol sequence gaps and terminal
errors itself. Gateway framing still supports browser `EventSource` with session cookies (spec §7).

## Optional editing

Use `new LiveResourceStore()` for an editor, or provide a policy that narrows the form's editable
regions. The store keeps delivered state and the draft separate: untouched fields follow incoming
updates, local changes stay, and differing concurrent edits become explicit conflicts. Use store
methods rather than mutating detached reads. See the [editor state model](../../docs/client-state-model.md).

A dirty draft, an accepted write and observed workload progress are separate states. A save captures
only the submitted intent; typing after Save must survive the result. The host performs all writes.

A quiet stream can still hold an older write version. See the
[visual explanation](../../docs/saving.md#why-a-quiet-stream-can-still-reject-a-save) and
[normative contract](../../spec/v1.md#6-ordering-delivery--the-state-guarantee).

`store.captureSave(uid)` captures a detached patch, UID and base resourceVersion together.
`store.captureReconciliation(uid)` guards a projected asynchronous response against newer watch state.
See the [complete conditional-save example](../../examples/conditional-save/README.md) and
[save guide](../../docs/saving.md) for host preconditions, real Kubernetes 409s, and draft preservation.

For Vue 3, use the [copyable composable](../../examples/vue/README.md) for reactive resource and connection state
with automatic subscription cleanup. Vue stays in the host application.
