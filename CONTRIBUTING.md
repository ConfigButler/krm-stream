# Contributing

## Get running

Open the repo in the devcontainer (*Reopen in Container*). Everything is already installed — Go 1.26,
Node 22, Task, jq/yq, k3d, kubectl.

```bash
task            # what there is
task test       # go test + node --test, both against the shared conformance fixtures
task lint       # go vet + golangci-lint + tsc --noEmit
task fixtures   # rebuild conformance/gen/*.json from the YAML sources
```

**Before every commit:** `task test` and `task lint` must pass, and `conformance/gen/` must not be
stale (`task fixtures-check`). CI enforces all three.

## The one rule

```
krm-stream  ──depends on──▶  nothing of ConfigButler's.  Ever.
```

This library knows **KRM**. It does not know GitOps, Flux, Dex, kcp, tenants, or ConfigButler. If the
gateway ever seems to need "which namespace is this caller's?", that is a missing *interface*, not a
missing import — inject it (`Authorizer`, `ClientFor(target, principal)`) and let the host answer.

The moment that rule bends, this stops being a library anyone else can adopt, and becomes a piece of
someone's product with a misleading name.

## The order of work

The contract comes first, and it is already written. Both implementations are built *against* it.

1. **[`spec/v1.md`](spec/v1.md)** — the wire. Normative; changes here are protocol changes.
2. **[`conformance/`](conformance/)** — the spec, executable. KRM object bodies in YAML, scenarios that
   say what the gateway must emit and what the client must then hold. **Both suites load the same
   files** — that is the whole reason this is one repo.
3. **[`packages/krm-stream/`](packages/krm-stream/)** — the client (`LiveResourceStore`). Pure logic,
   no Kubernetes, fastest feedback. Implement the algorithm in
   [`docs/client-state-model.md`](docs/client-state-model.md) until the fixtures are green.
4. **[`gateway/`](gateway/)** — the Go producer. Test against a fake watch; then prove it against a
   real API server (`task spike-up`).

**Test-first is not a style preference here.** The merge logic this library replaces shipped three
bugs in three days while living inline in an HTML file where no test could reach it. Each of those
bugs is now a named fixture (`edit-vs-unrelated-change`, `conflict-and-converge`, `dotted-label-keys`).
They exist so the rewrite cannot reintroduce them.

## Changing the protocol

A protocol change is a change to `spec/v1.md` **and** to the fixtures **and** to both implementations,
in one commit. CI is set up so that a change to `conformance/` re-runs both suites; if only one side
compiles, you have broken the contract and the build will say so.

Adding an *optional* event type or field is not a breaking change — consumers must ignore what they
don't know (spec §0). Anything else gets a new path segment (`/v2`), not a silent redefinition.

## Adding a fixture

1. Add or reuse a body in `conformance/bodies/` — it is a plain Kubernetes object, written the way you
   would `kubectl apply` it.
2. Add `conformance/fixtures/<id>.yaml`. In `why:`, name the rule it defends. If it defends no rule,
   ask whether it earns its keep.
3. `task fixtures && task test`. Commit the YAML **and** the generated JSON.

## Style

- **Go:** `gofmt`, `go vet`, `golangci-lint` clean. The wire types stay dependency-free — no client-go
  in `event.go`.
- **TypeScript:** `strict`, `biome check` clean. **No runtime dependencies, ever** — the published
  bundle is plain ESM a browser can `import` with no bundler, and that rule is absolute.

  **devDependencies are a different rule, and it is not "none".** They ship nothing, so the test is
  whether they pay for themselves. Three do:

  | | why |
  |---|---|
  | `typescript` | the compiler |
  | `@types/node` | without it `tsc` cannot see `test/` **at all** — the tests import `node:test`. `node --test` *strips* types, it does not check them, so `tsconfig.test.json` is the only thing standing between us and an unverified conformance suite |
  | `@biomejs/biome` | lint + format in one binary: the TypeScript half of what `gofmt`/`go vet`/`golangci-lint` do for the gateway. `task fmt-client` fixes what it can |

  Two tsconfigs, and the difference matters: `tsconfig.json` is the **build** (`src/` only — its
  `rootDir` is what ships); `tsconfig.test.json` is the **check** (`src/` + `test/`, `noEmit`). Both
  run in `task lint` and in CI.
- Comments explain *why*, and especially *what breaks if you do the obvious thing instead*. The
  fixtures are the best example of the house style: every one of them says what it defends.
