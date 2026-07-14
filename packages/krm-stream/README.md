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

The unscoped `krm-stream` package is a compatibility forwarder. This project is pre-1.0 and has not
been published to npm yet. See the repository [README](../../README.md),
[client state model](../../docs/client-state-model.md), and [release guide](../../docs/releasing.md).
