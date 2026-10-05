# Upgrading from 0.7

The release after 0.7 has one browser connector and one name for each gateway setting. Old names are
removed rather than kept as aliases, so the compiler or a failed import finds every call site. The
editor store's methods (`setValue`, `captureSave`, `captureReconciliation`, `adoptSaved` and so on)
are unchanged.

## Browser client

`connectResourceStream` is now the only connector. It takes a callback that receives each resource
state event, instead of a store:

| 0.7 | Now |
|---|---|
| `connectManagedResourceStream(url, store, options)` | `connectResourceStream(url, event => applyStreamEvent(store, event), options)` |
| `connectResourceStream(url, store, options)` (one connection) | the same: there is one connector, with bounded retries |
| `connectWithEventSource(url, store, options)` | the same: fetch sends the session cookie |
| `onStateChange: state => …` | read `connection.state`, then `connection.subscribe(state => …)` |
| `onChange: change => …` | render from `applyStreamEvent`'s return value inside the callback |
| `onSynced: () => …` | a `synced` event in the callback, or the `live` state |
| `onOpen: () => …` | the `syncing` state |
| `onGap: (expected, received) => …` | `state.gap` on the `retrying` state that the gap caused |
| `ManagedStreamOptions`, `StreamOptions` | `ResourceStreamOptions` |
| `ManagedStreamHandle`, `StreamHandle` | `ResourceStreamHandle` |
| `onError` | unchanged |

```js
const store = new LiveResourceStore();
let firstSnapshot;
const loaded = new Promise((resolve) => (firstSnapshot = resolve));

const connection = connectResourceStream(
  url,
  (event) => {
    render(applyStreamEvent(store, event));
    if (event.type === "synced") firstSnapshot(); // replaces onSynced
  },
  { onError: showError },
);
showConnection(connection.state);
const stopShowing = connection.subscribe(showConnection); // replaces onStateChange
connection.closed.catch(reportApplicationError);

// On teardown:
stopShowing();
connection.close();
```

Three behaviors to account for:

- **`closed` can reject.** If the callback, a `subscribe` callback or `onError` throws, the stream
  stops without retrying and `closed` rejects with that exception. Attach `.catch` (or await it);
  otherwise a rendering bug becomes an unhandled rejection.
- **The callback must be synchronous.** An `async` function type-checks, but it returns before
  applying the event, so `live` can be published before the snapshot is applied, and its exceptions
  never reach `closed`.
- **A protocol mismatch is terminal.** A stream whose `X-KRM-Stream-Protocol` header names another
  version is refused with a terminal `INTERNAL` error. Keep a vendored client in step with the
  gateway it talks to.

`applyStreamEvent` takes the callback's events, which carry no `seq` and are never errors. A host
that feeds a store from its own transport passes the same shapes: `reset`, `added` or `modified`
with `object`, `deleted` with `identity`, and `synced`.

## Go gateway

`Options` and `Gateway` embed one `StreamConfig`, so the settings they share are written inside it:

```go
gateway.Handler(gateway.Options{
	Principal: userFromSession,
	Scopes:    allowedScopes,
	StreamConfig: gateway.StreamConfig{
		Authorizer:   authorizeScope,
		Clients:      clientsForUser,
		Projections:  gateway.StaticProjection(gateway.ProjectionFull),
		WriteTimeout: 10 * time.Second,
	},
})
```

Assignments keep working through Go's field promotion: `options.Authorizer = …` and
`g.WriteTimeout = …` need no change.

| 0.7 | Now |
|---|---|
| `gateway.Gateway{Auth: a}` | `gateway.Gateway{StreamConfig: gateway.StreamConfig{Authorizer: a}}` |
| `Projection: p` on `Options` or `Gateway` | `Projections: gateway.StaticProjection(p)`; nil means `ProjectionFull` |
| `g.ServeStream(w, r, principal, scope)` | `g.ServeStream(w, r, principal, scope, "")` |
| `g.ServeStreamProjection(w, r, principal, scope, requested)` | `g.ServeStream(w, r, principal, scope, requested)` |
| `g.Stream(ctx, principal, scope, sink)` | `g.Stream(ctx, principal, scope, "", sink)` |
| `g.StreamProjection(ctx, principal, scope, requested, sink)` | `g.Stream(ctx, principal, scope, requested, sink)` |
| `gateway.NewSharedBackend(upstream)` | `gateway.NewSharedBackend(upstream, gateway.SharedOptions{})` |
| `gateway.NewSharedBackendWithOptions(upstream, options)` | `gateway.NewSharedBackend(upstream, options)` |
| `gateway.HeartbeatInterval` | `StreamConfig.HeartbeatInterval`; zero means 20 seconds |
| `Scope.Query()` | the scope's fields in order: target, group, version, resource, namespace, name, labelSelector |

The repository's test harness is no longer exported: `Fixture`, `WatchOp`, `FixtureEvent`, `Corpus`,
`LoadCorpus`, `LoadConformance`, `ScriptedBackend` and `NewScriptedBackend`. Neither is
`SSESink.Comment`. None of them is needed to serve a stream.
