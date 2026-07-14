# @configbutler/krm-stream

`@configbutler/krm-stream` is the official dependency-free ESM client for consuming a KRM resource
stream in a browser or JavaScript application. It provides:

- `LiveResourceStore` for server state, local drafts, conflicts, redactions, and merge patches.
- `connectWithEventSource` for same-origin browser streams.
- `connectResourceStream` for fetch-based transports with explicit headers.
- `resourceStreamURL` for the v1 scope query format.

The package is headless and does not choose a UI framework. It works with the Go gateway in this
repository or any conforming producer.

```ts
import { LiveResourceStore, connectWithEventSource, resourceStreamURL } from "@configbutler/krm-stream";

const store = new LiveResourceStore();
connectWithEventSource(
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
import { LiveResourceStore, connectWithEventSource } from "/krm-stream/krm-stream.js";
```

Both entry points are built from the same source and are exercised by the same browser test suite
against a real `EventSource`. Neither has a runtime dependency.

## Status

The unscoped `krm-stream` package is a compatibility forwarder. This project is pre-1.0: the protocol
and the API may still change before 1.0. See the repository [README](../../README.md),
[client state model](../../docs/client-state-model.md), and [release guide](../../docs/releasing.md).
