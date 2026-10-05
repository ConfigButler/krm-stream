# Contributing

## Prerequisites

The devcontainer provides Go 1.27.1, Node 24, Task, `kubectl`, and `k3d`.

```bash
task fixtures-check  # regenerate and verify shared fixture output
task test            # gateway, Kubernetes adapter, and client suites
task lint            # Go vet, golangci-lint, TypeScript, and Biome
task build-client    # produce the dependency-free ESM bundle
```

Run `task fixtures-check`, `task test`, and `task lint` before opening a pull request.

## Design rules

Start with watch streams and authoritative resource views; add editing as an optional layer.
The [README](README.md) explains current capabilities and how to choose a native or gateway source.
Current API examples must match this branch. Native viewing is LIST/WATCH on the shared
connection lifecycle and resumes ordinary reconnects from a per-handle checkpoint, and native editing
writes conditional merge patches back through the same proxy; the comparison example measures the
sources side by side. Streaming lists, pagination and editor cleanup are separate. Save-progress evaluation remains a proposal until implemented.

- Keep the core gateway free of `client-go`; Kubernetes integration belongs in `gateway/kube`.
- Keep the browser client framework-free and free of runtime dependencies.
- Keep credentials, application identity, authorization policy, and writes in the host application.
- Keep one current name for each API. Before 1.0, remove superseded names and forwarding packages;
  update callers and document the replacement instead of adding compatibility shims.
- Add public surface only for a demonstrated use case within the library’s scope. Prefer composing
  existing primitives, and test the observable guarantee each addition promises.
- Treat `spec/v1.md` and `conformance/` as the shared contract between gateway and client.

## Changing behavior

1. Update [spec/v1.md](spec/v1.md) when the wire contract changes.
2. Add or update a fixture in [`conformance/`](conformance/) for behavior both sides share.
3. Add focused package tests for behavior a fixture cannot express.
4. Run the validation commands above.

Protocol compatibility is explicit. Additive optional fields are allowed when consumers can ignore them;
incompatible event semantics require a new protocol version rather than a silent reinterpretation.

## Documentation changes

Keep viewing and transport guidance in [watching resources](docs/why-a-gateway.md), editor behavior in
[the editor model](docs/client-state-model.md) and writes in [saving](docs/saving.md). Examples own
their framework-specific guidance. Link to these contracts rather than repeating them in proposals.
Retain decisions and useful evidence when shortening completed plans; remove superseded request
inventories and duplicate guides. Preserve dated observations and generated release history.
Documentation-only changes need local link/anchor checks, snippet/API review and diagram review;
cluster and runtime suites are required when behavior changes, not for prose alone.

## Fixtures

Fixtures use source YAML in `conformance/bodies/` and `conformance/fixtures/`. Run `task fixtures` to
regenerate `conformance/gen/`; generated files are committed. Each fixture's `why` field should name
the rule it protects.

## Test levels

| Command | Purpose |
|---|---|
| `task test` | Deterministic gateway, adapter, client, and shared-fixture coverage. |
| `task e2e-wire` | Real Go SSE bytes consumed by the TypeScript client over HTTP. |
| `task e2e-browser` | Native `EventSource`, unbundled ESM and the native viewer and editor pages in Chromium. |
| `task cluster-facts` | Record observed Kubernetes behavior for the supported cluster version. |
| `task test-real-api` | Compose the save path and native viewing and editing against a real API server: gateway or host proxy, host endpoint and browser store. |
| `task compare-measure` | Measure native, full and spec sources, unshared and shared, under identical workloads against a real API server; `task compare-native-baseline BASELINE_REF=<sha>` compares the native connector with an earlier build. Results are evidence for the run, recorded in `docs/facts/`. |
| `task test-cluster` | Exercise the Kubernetes backend against a real API server. |

The cluster tasks need Docker and take longer; the fixture suites are the per-pull-request baseline.

## Style

- Format Go with `gofmt`; keep `go vet` and `golangci-lint` clean.
- Keep TypeScript strict and pass `biome check`.
- Prefer narrow changes and comments that explain constraints or non-obvious choices.
