# Upgrading from 0.7

The release after 0.7 has one browser connector and one name for each gateway setting. Old names are
removed rather than kept as aliases, so the compiler or a failed import finds every call site. The
editor store's methods (`setValue`, `captureSave`, `captureReconciliation`, `adoptSaved` and so on)
are unchanged on this branch. Proposed editor removals in proposal 0009 are not migration requirements
yet, and the one edit-policy change is [below](#editor-store). The native connector,
`connectNativeWatch`, and `nativeObjectURL` for native editing are new additions rather than
migration steps, and use today's standalone state-input helper. Gateway pages keep
`connectResourceStream`.
For the overall direction, see [watching resources](why-a-gateway.md).

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

### Editor store

`metadata.managedFields` and the `kubectl.kubernetes.io/last-applied-configuration` annotation are
read-only under every policy, as they already were on the gateway's side through
`ValidateMergePatch`. A map that holds one of them, or a redacted value, is now edited key by key:

| 0.7 | Now |
|---|---|
| `setValue`/`removeKey` on `["metadata", "annotations"]` as a whole | refused as read-only; set or remove each annotation key instead |
| `addKey`/`setValue` for the last-applied annotation, or anything under `managedFields` with a policy that made it editable | refused as read-only |
| `isEditable(uid, ["metadata", "annotations"])` | `false`; `isDirty` on it still reports edits underneath |
| A new key in a Secret's `data` beside redacted values was accepted but never reached `changes()` or `patch()` | it is an ordinary edit in the patch and survives later events |

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
