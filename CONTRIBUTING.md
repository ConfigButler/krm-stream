# Contributing

## Prerequisites

The devcontainer provides Go 1.26, Node 22, Task, `kubectl`, and `k3d`.

```bash
task fixtures-check  # regenerate and verify shared fixture output
task test            # gateway, Kubernetes adapter, and client suites
task lint            # Go vet, golangci-lint, TypeScript, and Biome
task build-client    # produce the dependency-free ESM bundle
```

Run `task fixtures-check`, `task test`, and `task lint` before opening a pull request.

## Design rules

- Keep the core gateway free of `client-go`; Kubernetes integration belongs in `gateway/kube`.
- Keep the browser client framework-free and free of runtime dependencies.
- Keep credentials, application identity, authorization policy, and writes in the host application.
- Treat `spec/v1.md` and `conformance/` as the shared contract between gateway and client.

## Changing behavior

1. Update [spec/v1.md](spec/v1.md) when the wire contract changes.
2. Add or update a fixture in [`conformance/`](conformance/) for behavior both sides share.
3. Add focused package tests for behavior a fixture cannot express.
4. Run the validation commands above.

Protocol compatibility is explicit. Additive optional fields are allowed when consumers can ignore them;
incompatible event semantics require a new protocol version rather than a silent reinterpretation.

## Fixtures

Fixtures use source YAML in `conformance/bodies/` and `conformance/fixtures/`. Run `task fixtures` to
regenerate `conformance/gen/`; generated files are committed. Each fixture's `why` field should name
the rule it protects.

## Test levels

| Command | Purpose |
|---|---|
| `task test` | Deterministic gateway, adapter, client, and shared-fixture coverage. |
| `task e2e-wire` | Real Go SSE bytes consumed by the TypeScript client over HTTP. |
| `task e2e-browser` | Native `EventSource` and unbundled ESM in Chromium. |
| `task cluster-facts` | Record observed Kubernetes behavior for the supported cluster version. |
| `task test-cluster` | Exercise the Kubernetes backend against a real API server. |

The cluster tasks need Docker and take longer; the fixture suites are the per-pull-request baseline.

## Style

- Format Go with `gofmt`; keep `go vet` and `golangci-lint` clean.
- Keep TypeScript strict and pass `biome check`.
- Prefer narrow changes and comments that explain constraints or non-obvious choices.
