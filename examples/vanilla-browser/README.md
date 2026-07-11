# examples/vanilla-browser — watch `status` reconcile, live

**The demo that sells the thing.** Point it at any object in any cluster and watch its `status` move —
conditions flipping, `readyReplicas` climbing, `observedGeneration` catching up — in a browser, with
real RBAC, and no save path at all.

No framework. No bundler. One `<script type="module">` that imports the built ESM directly, which is
the constraint the whole client is designed around.

```html
<script type="module">
  import { LiveResourceStore, connectResourceStream } from "./krm-stream.js";

  const store = new LiveResourceStore();
  connectResourceStream("/resource-stream/v1?group=apps&version=v1&resource=deployments&namespace=app&name=web", store);
  store.subscribe(() => renderStatus(store.status(uid)));
</script>
```

**Not built yet.** It lands once the client and the gateway are green against the conformance suite —
it is the proof, not the prototype. See [`CONTRIBUTING.md`](../../CONTRIBUTING.md) for the order of
work.

Why status-only, and why first: it is the one use case that needs *no* merge, *no* conflict model and
*no* write path — so it is the smallest honest demonstration that the stream is real. If watching a
controller reconcile in a browser is not compelling on its own, nothing built on top of it will be.
