# Vanilla browser example

The example renders the official `@configbutler/krm-stream` ESM client in a browser with native
`EventSource`. It runs against the replay gateway and the shared conformance corpus; no Kubernetes
cluster is required.

```bash
task demo
# http://127.0.0.1:8100/?fixture=status-only-churn&pace=800ms
```

Use `task e2e-browser` to run the Chromium check.

The page demonstrates live status updates, three-way conflict handling, draft edits, and redacted
Secret values. Useful fixtures include:

| Fixture | Demonstrates |
|---|---|
| `status-only-churn` | Status updates while an editable draft remains intact. |
| `conflict-and-converge` | A real conflict followed by server convergence. |
| `edit-vs-unrelated-change` | An unrelated server update preserves the local edit. |
| `secret-redaction` | Redacted values remain unavailable and read-only. |
| `named-object-absent` | An empty snapshot is a valid resource state. |

Add `pace=0ms` for the fastest replay or use a positive value to inspect each event.
