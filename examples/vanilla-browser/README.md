# examples/vanilla-browser — watch `status` reconcile, live

**The demo that sells the thing**, and the only test of the constraint the whole library is designed
around.

```bash
task demo      # http://127.0.0.1:8100/?fixture=status-follow-live&pace=800ms
```

No cluster. The [replay gateway](../../gateway/cmd/replay) serves the conformance corpus as a real SSE
stream, so every scenario the suites assert is one you can *watch* — a status rolling out, a conflict
raised and then converging, a Secret whose value you can see is there and can never read.

```bash
task e2e-browser   # the same thing, asserted: a real Chromium, in CI
```

## What this page is for

It is not a prototype. It is the rung of the test ladder that nothing below it can reach:

| it proves | why nothing else can |
|---|---|
| the published ESM **imports in a browser with no bundler** | Node importing `dist/index.js` proves nothing — Node is not a browser. Here the browser fetches `index.js`, which imports `./store.js`, which imports `./merge.js`… one bare specifier or one missing extension and the page is blank |
| **native `EventSource`** works | it is the same-origin session-cookie path, and the v1 baseline (spec §7). `fetch` cannot test it, because it is a different transport |
| a read-only region actually **flashes** | "read-only is not ignored" is the product thesis, and it is a **DOM** fact, not a data-structure fact |
| you can **type while the object changes underneath you** | the three-way merge, from the only end that matters |

Everything is served from **one origin** — the stream, the built ESM at `/krm-stream/`, and this page.
That is not a convenience: it is the deployment the protocol is specified around, and a demo on a
second port would need CORS and would then be proving something we do not ship.

## The page

One `<script type="module">`, no framework, no build step of its own:

```js
import { LiveResourceStore, connectWithEventSource } from "/krm-stream/index.js";

const store = new LiveResourceStore();
connectWithEventSource(`/resource-stream/v1?fixture=${fixture}`, store, {
  onChange: (flashed) => { highlight(flashed); render(); },
});
store.subscribe(render);
```

`render()` reads `store.status(id)` (read-only, follows the server), `store.draft(id)` (editable,
three-way merged), `store.isDirty`, `store.conflicts(id)` and `store.patch(id)`. That is the entire
API surface a host needs.

## Fixtures worth watching

| `?fixture=` | what you see |
|---|---|
| `status-follow-live` | the headline: `readyReplicas` climbs and `Available` flips while you edit `spec.replicas`, and your edit is untouched |
| `status-only-churn` | the spec-only projection: status churn is suppressed and never disturbs the draft |
| `conflict-and-converge` | type `debug`; the server says `warn` (a conflict, your text kept); then the server *arrives* at `debug` and the conflict clears itself |
| `edit-vs-unrelated-change` | the server bumps a key you are **not** editing, and nothing of yours moves |
| `secret-redaction` | the keys of a Secret are disclosed, the values are masked, and there is no input to type into |
| `named-object-absent` | an object that does not exist: empty, not a ghost, not an error |

Add `&pace=800ms` to watch it at human speed; `&pace=0ms` is as fast as the socket will carry it.
