# @configbutler/krm-stream

`@configbutler/krm-stream` is the official dependency-free ESM client for live Kubernetes resource
state in a browser or JavaScript application, with optional editing. It provides:

- `connectNativeWatch`, which reads a native Kubernetes collection through a host proxy with LIST
  and WATCH, and `nativeCollectionURL` to address one.
- `connectResourceStream`, which reads a gateway's projected views over SSE, and
  `resourceStreamURL` for the v1 scope query format.
- `LiveResourceStore` for server state, local drafts, conflicts, redactions, and merge patches, and
  `applyStreamEvent` to apply a state event to it.

Both connectors use fetch, share one connection lifecycle and hand each resource state event to a
callback, with bounded recovery. Native access delivers original authorized resources; the gateway
adds projected views, redaction, suppression and optional upstream watch sharing. Start with a
watch-backed list or viewer; add draft reconciliation only when the page needs editing. The package
is headless and does not choose a UI framework. Native viewing is read-only in this release.

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

See [native connections](#native-connections) for the same viewer over a host's Kubernetes proxy.

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

`connectResourceStream` is the gateway connector. It uses fetch for same-origin cookies or
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

### Native connections

```ts
const connection = connectNativeWatch(
  nativeCollectionURL("/k8s", { version: "v1", resource: "configmaps", namespace: "app", name: "settings" }),
  event => applyStreamEvent(store, event), // a LiveResourceStore(readOnlyPolicy) for this source only
  { onError: (code, message, terminal) => showStreamError(code, message, terminal) },
);
```

`connectNativeWatch` takes the same options and returns the same handle, states and completion
guarantees as `connectResourceStream`. `nativeCollectionURL` builds core (`/api/v1`) and grouped
(`/apis/<group>/<version>`) collection paths, namespaced or cluster-wide, with an optional
`labelSelector`; `name` becomes `fieldSelector=metadata.name=<name>`, so a named object can be absent,
deleted and recreated like any member. The host mounts the API server under the base, such as `/k8s`,
and owns credentials, routing and authorization; the collection URL must not set `watch`,
`resourceVersion`, `limit` or `continue`, which the connector controls.

Each connection performs an ordinary LIST, validates every item's UID and name, and fills a missing
item `apiVersion`/`kind` from a typed collection (`ConfigMapList` lists `ConfigMap`s; type metadata
already present is kept). It delivers `reset` and an `added` per member, then opens a WATCH from the
collection's `resourceVersion` with the same selectors, and delivers `synced` — publishing `live` —
only once that WATCH is accepted. Watch events map to `added`, `modified` and `deleted` (with the
identity of the deleted object); bookmarks change nothing. Every reconnect lists again, so prior
state is kept until a replacement snapshot completes and prunes what it no longer lists: deletes and
selector exits missed while disconnected, or a same-name object recreated with a new UID.

HTTP or in-stream 410 (expired watch history), 408, 429, 5xx, network failures, EOF, and malformed or
truncated frames are reported where applicable and consume the bounded retry budget; repeated expiry
cannot re-list in a tight loop. A Kubernetes `Status` message reaches `onError`, and its
`retryAfterSeconds` or an HTTP `Retry-After` sets the least the next reconnect waits. HTTP or in-stream
401, 403 and other 4xx are terminal. This release does not paginate: a LIST with a continuation token
is refused with a terminal error before anything is applied, because pruning from one page would
remove the others. Native objects carry whatever the proxy returns, including Secret values and
machinery fields; there is no projection, redaction or `seq`. Never fall back to native access after a
gateway refuses a view.

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
