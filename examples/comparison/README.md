# Native, krm-full/v1 and krm-spec/v1 compared

One host serves **the same objects** through every source krm-stream offers, from one origin, so a
page and a measurement driver can compare them under identical objects and identical churn. It
composes the existing connectors, stores, editors and recipes; it adds no library API.

| Source | Route | What reaches the browser |
|---|---|---|
| native | `/k8s/…` host proxy | The API server's own LIST and WATCH responses, Secret values and machinery included. Writes are conditional merge PATCHes through the same proxy. |
| full | `/stream/full` | `krm-full/v1` over SSE: machinery removed, Secret values withheld (paths and redaction revisions only), bookkeeping-only updates suppressed. One upstream watch per stream. |
| spec | `/stream/spec` | `krm-spec/v1`: as full, and `status` omitted, so status-only updates are suppressed. One upstream watch per stream. |
| full-shared, spec-shared | `/stream/full-shared`, `/stream/spec-shared` | The same views through `gateway.NewSharedBackend`: one upstream watch per scope, shared by every subscriber. |

Gateway editing goes through host save endpoints, `/save/{full,spec}/widgets/{name}`, built like
[conditional-save](../conditional-save/README.md): a projected GET for guarded recovery reads and a
PATCH checked by `gateway.ValidateMergePatch` for that projection, carrying the captured UID and
resourceVersion as preconditions. Native editing uses the [native editor](../native-editor/README.md)
through the proxy, which forwards only PATCHes `gateway.ValidateNativeMergePatch` accepts.

The objects live in a scratch namespace the host creates and deletes: 8 Widgets (a CRD with `spec`
and a real `status` subresource — the definition the real-API suite installs) and 3 Secrets.

## Files

- [gateway/kube/examples/comparison](../../gateway/kube/examples/comparison/) — the host: native proxy
  ([native.go](../../gateway/kube/examples/comparison/native.go)), the four stream routes with a
  counting `Backend` wrapper ([gateway.go](../../gateway/kube/examples/comparison/gateway.go)), save
  endpoints ([save.go](../../gateway/kube/examples/comparison/save.go)), deterministic workloads
  ([workload.go](../../gateway/kube/examples/comparison/workload.go)), counters
  ([metrics.go](../../gateway/kube/examples/comparison/metrics.go)) and
  [cmd/compare](../../gateway/kube/examples/comparison/cmd/compare/main.go). Its unit test needs no
  cluster: schedules, the proxy's policy and counters, and the gateway route's refusal.
- [index.html](index.html) and [page.ts](page.ts) — the page. [build.mjs](build.mjs) compiles
  `page.ts`, with the native editor, the conditional editor and the keep-local recipe it reuses, into
  `dist/page.js`, importing the library as `@configbutler/krm-stream` through the page's import map.
- [measure.ts](measure.ts) — the measurement driver, run by node with types stripped.
  [measure.sh](measure.sh) starts the host, runs it and stops the host.
- [tsconfig.json](tsconfig.json) — typechecks both TypeScript files with the client package's
  compiler: `packages/krm-stream/node_modules/.bin/tsc -p examples/comparison`.

## Run the page

```bash
task compare        # needs the spike cluster: `task cluster-up`
```

Open **https://127.0.0.1:8111/** and accept the self-signed certificate (made at startup, for
loopback only). The host also serves plain HTTP/1.1 on `http://127.0.0.1:8110/`, but a browser
allows six HTTP/1.1 connections per origin and this page holds six streams (two collections per
source), so over HTTP/1.1 its saves and metrics polls would queue behind them. TLS gives the browser
HTTP/2, which multiplexes them. `/` signs in as the viewer session (a cookie) and opens the page.
Add `?entry=bundle` for the single-file bundle and `?variant=shared` for the shared gateway routes.

The page shows, per source: connection state, a table of the Widgets, an editor for the selected
Widget's `spec.note` and `spec.replicas`, conflicts with "Take theirs" (`store.revert`) and "Keep
mine" (the [keep-local recipe](../editor-recipes/keepLocal.ts)), the Secrets, and counters — events
by type, store notifications, renders (one per animation frame in which anything changed), bytes the
connector read and reconnects. Below them are the host's own metrics, controls to run a workload,
make a competing write to the selected Widget, or force-disconnect native, gateway or all downstream
connections, and a panel that opens the gateway as the refused session.

Switching between unshared and shared opens new stores: a store is never reused across sources,
routes or identities.

## Measure

```bash
task compare-measure                                   # the matrix: ~10 minutes
task compare-measure -- --workloads churn --subscribers 1,25 --reps 5 --duration-ms 30000
task compare-native-baseline BASELINE_REF=<commit>     # native before/after
```

`compare-measure` starts the host on `127.0.0.1:8110` (plain HTTP; the driver is node), runs
[measure.ts](measure.ts) against this tree's built library, and writes JSON and a markdown table to
`examples/comparison/results/`. For every workload, subscriber count N and repetition it opens N
subscribers per source — each watches the Widgets and the Secrets, one connection and one store per
collection — waits until all are live, runs the workload through the host, and then requires every
store to converge on the cluster's state. That is the correctness gate: a native store must hold
each object's exact resourceVersion, spec, status and Secret data; a projected store must hold the
same membership, spec (and status, for full), no Secret values, and exactly the cluster's Secret keys
as redacted paths. A run that does not converge within `--settle-ms` is reported as failed and left
out of the tables. All five sources run concurrently in each run, so every source sees the same
writes; pass `--sources` to isolate one.

`compare-native-baseline` builds the library as it is at `BASELINE_REF` (default `main`) into a
scratch directory, measures the native source against it and then against this tree's library with
the same options, and writes a before/after table. Name the commit **before** the change under test:
once `main` contains it, `main` is not a baseline. Every option of `measure.ts` is documented at the
top of the file; `--lib DIR` and `--entry index|bundle` load any built library.

### Workloads

Deterministic for a given duration, rate and seed, and identical for every source. Every workload
includes forced reconnects — 2 by default, spread evenly — so reconnect cost is measured under each.

| Workload | Writes | Forced disconnects |
|---|---|---|
| quiet | none | the 2 evenly spread |
| burst | every 4 s, 12 writes 20 ms apart, half spec and half status (`--rate` is writes per burst) | the 2 evenly spread |
| churn | status-only writes at 10/s, a spec write every 2 s, a Secret rotation every 3 s | the 2 evenly spread |
| forced-disconnect | mixed writes at 4/s (¼ spec, ½ status, ¼ Secret) | every 4 s, plus the 2 |

A disconnect closes every downstream native watch and SSE stream, as a dropped connection would; it
never touches the upstream API-server watches directly. The driver sets the connector's
`retryDelayMs` to 0 (by default) so time-to-live measures reconnect work rather than backoff jitter,
and `healthyResetMs` to 1 s so frequent forced disconnects never exhaust the retry budget.

## What each counter means

Host counters (`GET /metrics`, reset with `POST /metrics/reset`; both need the viewer session). Every
key is built from constants, a route name, an event type or an HTTP status — never a UID, user,
namespace or resourceVersion.

| Key | Meaning |
|---|---|
| `native.list_requests` | LIST requests forwarded. |
| `native.watch_requests` | WATCH requests forwarded. |
| `native.watch_after_list` / `native.watch_resumed` | A WATCH that followed a LIST for the same collection and session, or one with no LIST waiting for it: a watch resumed from a checkpoint the browser kept. The pairing is per collection and session, so it is exact in aggregate, not per tab. |
| `native.watch_streaming_list`, `native.watch_bookmarks_requested`, `native.watch_without_resource_version` | The watch parameters the connector chose. |
| `native.watch_frames.{ADDED,MODIFIED,DELETED,BOOKMARK,ERROR}` | Watch frames the proxy passed to browsers. |
| `native.bytes.{list,watch,get,patch}`, `native.upstream_bytes` | Bytes to browsers per request class; bytes read from the API server. |
| `native.{get,patch}_requests`, `native.*_status.<code>` | Editor reads and writes, and their answers. |
| `native.refused_session_requests` | Requests made by the refused session. After a gateway refusal it must stay 0. |
| `gateway.<route>.upstream_watch_calls`, `…upstream_watches_active` (gauge) | Watches the route's own kube Backend opened, counted by a wrapper beneath any SharedBackend. Nothing else using the cluster can move them, unlike API-server gauges. |
| `gateway.<route>.upstream_bytes` | Bytes the route's client read from the API server. |
| `gateway.<route>.obs.<kind>[.<type>]` | Gateway Observations: `cycle_started`, `consumer_resync`, `event_emitted.<type>`, `event_suppressed.<type>`, `shared_subscription_opened`, `stream_opened`, errors by code. |
| `gateway.<route>.authorizer_calls.{cycle,timed}`, `…authorizer_refusals` | Authorizer calls: at each snapshot cycle (opening or resync) and the timed rechecks (every 5 s here, `--reauthorize-every`). |
| `gateway.<route>.sse_bytes`, `…sse_active` (gauge) | Bytes of SSE written to browsers; open streams. |
| `native.session_checks`, `gateway.<route>.session_checks`, `sessions.<path>.<who>` | Host session checks: one per native request, one per gateway stream request. |
| `save.{full,spec}.*` | Save endpoint GETs, PATCHes, answers and bytes. |

Driver and page counters: **events** by type as the consumer received them; **store
notifications** (`store.subscribe` callbacks); **renders** — in the driver, one per macrotask in
which the store notified (the first notification after the previous render schedules the next on
`setTimeout(0)`), in the page one per animation frame; **bytes** the connector read, counted by a
wrapping `fetch`; **reconnects** (transitions to `retrying`); **time-to-live**, from a connection
leaving `live` to being `live` again; **Secret values seen changing** (native) and **redaction rev
bumps** (full/spec) — a rotation is a value change natively and only a revision through the gateway.

Not measured: Kubernetes' own authorization of each forwarded native request (it happens inside the
API server, invisible to the host), browser paint and layout cost, memory and CPU of the host, the
driver or the API server, and API-server-side watch-cache work.

## Safety notes

- **A gateway refusal is final.** The page and the driver never retry a refused source through
  `/k8s`: native access has no projection, so it would disclose what the gateway withheld. The
  refused session's panel and `native.refused_session_requests` show it.
- **Native access discloses Secret values.** The native column shows them; full and spec name
  redacted paths and revisions only. Do not offer native access to Secrets a user may not read.
- **One store per source, scope and identity.** Two connections must never feed one store, and a
  store from one source must never be given another source's responses or editor.
- The host holds the only Kubernetes credential. Its admin and metrics routes are harness controls
  guarded only by the viewer session: keep it on loopback.

## Smoke checks

Recorded in [docs/facts/comparison-2026-10-05.md](../../docs/facts/comparison-2026-10-05.md): the page
driven headlessly in Chromium over HTTPS/HTTP/2 on both entry points — live state on every source,
Secret disclosure per source, a native edit and gateway edits (full and spec) saved and echoed,
later typing during a save, conflicts and their resolution, a forced disconnect recovering, the
shared routes, and the refused session ending terminally with no native request.
