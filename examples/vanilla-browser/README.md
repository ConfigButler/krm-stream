# Vanilla browser example

The example renders the official `@configbutler/krm-stream` ESM client in a browser with its fetch
connector, `connectResourceStream`. It runs against the replay gateway and the shared conformance
corpus; no Kubernetes cluster is required.

```bash
task demo
# http://127.0.0.1:8100/?fixture=status-only-churn&pace=800ms
```

Use `task e2e-browser` to run the Chromium check. It loads both published entry points, and also
reads the gateway with a raw native `EventSource` to check its framing, reconnect snapshots and
terminal shutdown.

The page demonstrates live status updates, three-way conflict handling, draft edits, and redacted
Secret values. Useful fixtures include:

| Fixture | Demonstrates |
|---|---|
| `status-follow-live` | Live status updates while an editable draft remains intact. |
| `status-only-churn` | Spec projection suppresses status updates while retaining edits. |
| `conflict-and-converge` | A real conflict followed by server convergence. |
| `edit-vs-unrelated-change` | An unrelated server update preserves the local edit. |
| `secret-redaction` | Redacted values remain unavailable and read-only. |
| `named-object-absent` | An empty snapshot is a valid resource state. |

Add `pace=0ms` for the fastest replay or use a positive value to inspect each event.

The example uses today's projected SSE connector over fetch. For a read-only page, render server
state with `LiveResourceStore(readOnlyPolicy)` as shown in the [README](../../README.md#watch-a-resource-view-today).
Editing is an optional layer; the [editor model](../../docs/client-state-model.md) and
[saving guide](../../docs/saving.md) describe its intended use. A small native viewer is the next
slice; native editing and a larger source comparison follow separately. They are not features of
this gateway replay demo.
