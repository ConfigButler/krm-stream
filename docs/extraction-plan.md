# `krm-stream` — extraction plan

> **Where this came from.** This repo was extracted from
> [ConfigButler/gitops-api](https://github.com/ConfigButler/gitops-api), whose browser console is both
> the code that motivated it and its first consumer. This is that extraction's plan and decision
> record — it lives here, next to what it decided, rather than in the repo it left.
>
> **Status:** Phases 0–1 **done** (2026-07-11) — this repo exists, and CI is green. What remains is the
> part that touches gitops-api: cutting its console over to these libraries.
>
> Name and packaging: [naming decision](naming.md) (settled — `krm-stream`, unscoped on npm). Specs
> this plan executes: [protocol](../spec/v1.md) · [client state model](client-state-model.md) ·
> [gateway](../gateway/README.md).

---

## 1. Why a monorepo (and not three repos)

The three artifacts are joined by **one contract**. Split them across repos and the producer (Go
gateway) and the consumer (TS client) drift the first time someone edits the protocol — which is the
exact failure the protocol spec exists to prevent.

One repo buys the thing that makes the contract enforceable:

> **`conformance/` — shared fixtures.** KRM object bodies in YAML, plus scenarios that say what the
> gateway must *emit* and what the client must then *hold*. **Both suites load the same generated
> JSON.** A protocol change that breaks one side fails both, in the same commit, before it can ship.

That is the whole justification. Everything else (one clone, one PR, one CI) is convenience.

**It earned its keep on the first run.** The corpus failed immediately — and the bug was in the *Go
wire types*, not in the test: `redactedPaths` carried `omitempty`, which drops the empty array. That
is precisely the case the spec makes mandatory (a consumer must never *infer* redaction from a value
that merely looks like a placeholder). A fixture caught a spec violation before a single line of
gateway existed. That is the entire thesis, demonstrated on day one.

**A correction to the original plan.** It assumed one fixture set that both suites could "load and
assert". That is impossible: a gateway is fed *watch ops* and produces *events*; a client is fed
*events* plus *edits* and produces a *draft*. So a fixture has three sections — `watch` (gateway
input), `events` (**the shared surface — the only genuinely shared part**), and `client` (edits +
expected outcome) — and each suite reads the parts that apply to it. They meet in the middle, at
`events`. Without that split, "both sides run the same fixtures" would have been theatre.

**We do not lose independent releases.** The client publishes to npm as `krm-stream`; the gateway is a
nested Go module with its own tags (`gateway/vX.Y.Z`). Consumers depend on one or the other, never
"the monorepo."

**Why now, and not "just keep it in gitops-api":** the reconcile engine shipped three bugs in three
days while living inline in an HTML file, unreachable by any test. The point of extraction is not
tidiness — it is that this logic must be *proven*, and it could not be proven where it lived.

---

## 2. What moves, what stays

The dividing line is one question: **does it know anything about gitops-api, ConfigButler, Dex, kcp, or
tenants?** If yes it stays. If it only knows *KRM*, it moves.

| Today (gitops-api) | Fate | Why |
|---|---|---|
| the three design docs | **moved** → `spec/v1.md`, `docs/client-state-model.md`, `gateway/README.md` | canonical contract; must live with the fixtures that enforce it |
| the reconcile/merge JS **inside** `cmd/server/console_body.html` (`applyServer`, `changesFor`, `patchFor`, dirty/conflict) | **moves** (as a **rewrite**) → `packages/krm-stream/` | pure KRM logic. Rewritten test-first against the corpus — copying it would import its bug history. Its three bugs are now fixtures: R-THREEWAY, R-DERIVED, R-ID |
| `cmd/server/console.go` — watch→SSE loop, `configMapView` reduction, merge-patch apply | **moves** (as a **generalisation**) → `gateway/` | the mechanics are sound; generalise ConfigMap→dynamic client, and `configMapView`→a complete projected KRM object (spec + status verbatim) |
| **the rest of** `console_body.html` — CSS, DOM rendering, the save pane, the SSE wiring | **stays** — but see the ⚠ in §5 | this is the *host*: a UI, not an engine |
| `cmd/server/console.go` — session gate, `consoleContext`, `kcpTenantForOwner`, `firstSelectionNamespace`, the kcp front-proxy hop, `userWorkspaceClientset` | **stays** | Dex, kcp, Tenant/RepoSelection — gitops-api's private business. These become the *injected* bits the gateway library asks for |
| `cmd/server/wizard.go`, `main.go`, the operator, `internal/*` | **stays** | untouched; nothing to do with KRM streaming |

**The dependency direction is one-way and must stay that way:**

```
gitops-api  ──depends on──▶  krm-stream (client + gateway)
krm-stream  ──depends on──▶  nothing of gitops-api's.  Ever.
```

If the gateway ever needs to know "which namespace is this caller's," that is a missing interface —
inject it (`Authorizer`, `ClientFor(target, principal)`), don't import gitops-api. The rule is written
into the new repo's CONTRIBUTING.md, where it will actually be read.

---

## 3. What was built (Phase 1, done)

```
krm-stream/
  README.md  LICENSE  CONTRIBUTING.md  Taskfile.yml
  .devcontainer/                 Go 1.26, Node 22, Task, yq/jq, k3d, kubectl
  .github/workflows/ci.yml       fixtures-are-fresh · go test+lint · node --test + tsc
  spec/
    v1.md                        the protocol (moved)
    events.schema.json           the same, machine-readable
  conformance/
    bodies/      12 KRM objects in YAML — ConfigMap, Deployment (with live status), Secret
    fixtures/    14 scenarios, each naming the rule it defends
    gen/         generated JSON — BOTH suites read this
    generate.sh
  gateway/                       Go → github.com/ConfigButler/krm-stream/gateway
    event.go  conformance.go  *_test.go  .golangci.yml  go.mod
  packages/krm-stream/           TS → npm `krm-stream` (unscoped)
    src/types.ts  test/  package.json  tsconfig.json
  examples/vanilla-browser/      the status-watch demo (deferred; see §5)
  docs/
    client-state-model.md        the merge algorithm (moved)
    extraction-plan.md           this document
    naming.md                    why `krm-stream`, and why not `krm-live`
```

**Tooling: nothing new to install.** Node 22 runs `.ts` under `node --test` natively (type stripping),
so the client suite has **zero test dependencies**; `typescript` is its only devDependency. Verified by
running it, not assumed.

---

## 4. Three hard constraints

Non-negotiable, because of how gitops-api consumes the result:

1. **The client emits plain, dependency-free ESM.** gitops-api has *no* bundler and is not getting
   one. `tsc` emits an ES2022 module a browser can `import` via `<script type="module">`. A runtime
   dependency in this package is a design smell. (`rewriteRelativeImportExtensions` is what lets the
   source import `./merge.ts` — which Node's type-stripping test runner requires — while the emitted
   JS imports `./merge.js`, which the browser requires. Without it, exactly one of the two works.)
2. **The gateway is a nested Go module** (`gateway/go.mod`) so `go get` works without pulling the TS
   side. Tags are `gateway/vX.Y.Z`.
3. **The repo is public, and that was a technical decision, not a philosophical one.** gitops-api's
   Dockerfile does `COPY go.mod go.sum` → `go mod download` with no netrc, no `GOPRIVATE` and no
   secret mount. A *private* `github.com/ConfigButler/krm-stream/gateway` would not resolve there —
   and a `replace ../krm-stream` points outside the Docker build context, so that fails too. Public,
   with a tagged `gateway/v0.1.0` before the Phase 4 image build, is the only path that doesn't add
   build secrets.

---

## 5. Migration phases

The governing rule: **the live console keeps working the entire time.** gitops-api is not touched until
a library is green and proven against the corpus.

> **⚠ The phase order is inverted from the original plan, and the original was wrong.**
>
> It cut the *browser* over first ("the lower-risk half"), leaving the old server in place. But the
> client speaks protocol v1 — lowercase `added`/`modified`, a complete KRM `object`, mandatory
> `redactedPaths`, `reset.projection`, a `deleted.identity` — while today's server emits uppercase
> `ADDED` carrying a flat `{labels, annotations, data}` view. That order therefore needs a throwaway
> adapter in both directions. Worse, it claimed "the DOM rendering and save pane stay exactly as they
> are", which is false: the renderer binds every input to `data-section` ∈ {labels, annotations, data}
> plus a flat `data-key`, while the client addresses fields by **segment-array path over nested KRM**.
> The DOM layer *is* the part that gets rewritten.
>
> Serving the new protocol from a **new endpoint alongside the old one** is strictly safer: the running
> console is untouched, and the browser flips once, against the real wire.

### Phase 0 — Preserve the record ✅
The design docs written and committed in gitops-api, where the work started. *Done.*

### Phase 1 — Seed the monorepo ✅
Repo created; protocol + schema + corpus + both skeletons + devcontainer; CI green. *Done.*

### Phase 2 — The client, test-first (no cluster) ← **next**
Encode the client half of the corpus as a real runner (replay `events`, apply `client.edits` at their
`after:` positions, assert `expect` and the `checkpoints`), watch it **fail**, then implement
`LiveResourceStore` until green: replace-not-merge, deep three-way reconcile over the editable regions,
read-only regions that follow the server live, derived dirtiness, segment-array paths, the merge-patch
builder, atomic arrays.
*Exit:* `task test` green; `tsc --noEmit` clean; the three historical bugs each have a passing
regression fixture.

*Why first:* pure logic, fastest feedback, no Kubernetes, and it is the piece with the bug history.

### Phase 3 — The gateway, against a fake watch (no cluster)
Lift `console.go`'s stream/save mechanics; generalise to a dynamic client and a complete projected KRM
object; add the `Authorizer` / `ClientFor(target, principal)` seams; implement the Secret disclosure
policy (keys-only) **and the patch guard that refuses a redacted path** — the client is not the
security boundary. Drive it from each fixture's `watch:` ops with `watch.NewFake()` and assert the
emitted `events:`.
*Exit:* `go test` green incl. the corpus; then proven against a real API server (`task spike-up`).

### Phase 4 — gitops-api: serve the new protocol **alongside** the old
Add `/console/resource-stream/v1` and a new apply endpoint, implemented with the gateway library.
`console.go` keeps its session gate, tenant/namespace resolution, kcp hop and (future) CommitRequest,
and injects those as the authorizer + client factory. **`/console/stream` and `/console/save` stay
exactly as they are, still serving the live console.** Needs the public repo + a `gateway/v0.1.0` tag
(§4.3).
*Exit:* both endpoints live; the console untouched and still green; the new one verified by hand.

### Phase 5 — gitops-api: flip the browser, in one commit
`console_body.html` becomes `type="module"`, imports the vendored ESM (`go:embed`ed, served at
`/console/krm-stream.js`), and renders from the store. The CSS and the save pane survive; **the
renderer is rewritten** against path-addressed KRM. Rollback is reverting one file, because the old
endpoints still exist.
- **⚠ Remember the Taskfile lesson:** add the vendored `.js` to the `image` task's `sources`, or the
  container ships a stale asset (this already bit us once).

*Exit:* the console behaves identically live — including the two-browser conflict case. **Then**
delete the old endpoints and the inline engine.

### Phase 6 — The demo that sells it
`examples/vanilla-browser/`: point it at any object, watch `status` reconcile. No save path. This is
the artifact that shows *why* the thing exists — and the only phase that needs the standalone
`krm-stream-gateway` binary + image.

---

## 6. Avoiding two copies of the spec

**Done.** This repo is canonical, and gitops-api keeps no second copy of anything. The three specs it
used to carry (`resource-stream-protocol.md`, `live-reconcile-engine.md`, `resource-stream-gateway.md`)
now live here as `spec/v1.md`, `docs/client-state-model.md` and `gateway/README.md`; this plan and the
naming decision came with them. What gitops-api keeps is a single pointer
(`docs/design/krm-stream.md`) saying where the family lives and what it owes it.

Two live copies of a contract is exactly the drift this whole repo is designed to prevent. Keeping one
in the *repo that motivated the extraction* would have been the least defensible place of all.

---

## 7. CI

Green on the first push, three jobs:

- **fixtures are the contract** — regenerates `conformance/gen/` and fails if it is stale
- **gateway (go)** — `go vet` · `go test` · `golangci-lint` (0 issues)
- **client (typescript)** — `node --test` · `tsc --noEmit` · `tsc`

gitops-api itself has **no CI at all** (it has no `.github/workflows`). So the "a protocol
change fails both suites" gate lives only upstream, and that leaves one gap on *our* side: the vendored
`krm-stream.js` in `cmd/server/` will have nothing checking that it matches the gateway version it is
paired with. Cheap fix at Phase 5 — stamp the vendored file with the client version and have
`task test` assert it matches the gateway version in `go.mod`. Otherwise we have reinvented drift on
our own side of the contract.

---

## 8. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Cutover breaks the working console | Phases 4 and 5 are separate; the old endpoints stay live through both and are deleted only after the new path is proven. Rollback in Phase 5 is one file |
| The client rewrite reintroduces an old bug | Each of the three historical bugs is a named fixture (R-THREEWAY, R-DERIVED, R-ID) that must fail before it can pass |
| Spec and code drift | Shared corpus; CI fails both sides on a protocol change |
| **Array merge is coarse** | Atomic-on-change ships first (client-state-model §4.1) — correct but coarse: your one-element edit conflicts with the server's unrelated append. The keyed merge (`x-kubernetes-list-map-keys`) is the known follow-up, and **the project's main open design question** — not a TODO |
| Stale embedded asset ships | The `image` task's `sources` must include the vendored `.js` — this already bit us once |
| The library grows a gitops-api dependency | The one-way rule (§2), written into the upstream CONTRIBUTING.md |
| Secrets leak through a generic KRM view | Keys-only disclosure, `redactedPaths` on the wire, **and a gateway-side patch guard** — a value never shown can never round-trip back on save |

---

## 9. Decisions — all closed (2026-07-11)

1. **Name** — `krm-stream`. See [the naming decision](naming.md); `krm-live` was rejected
   because it collides with `kpt live` in our exact domain, with our exact audience.
2. **Public or private** — **public**, and §4.3 is why: private breaks the Docker build.
3. **Repo** — created: [ConfigButler/krm-stream](https://github.com/ConfigButler/krm-stream), seeded,
   CI green.
4. **npm** — **`krm-stream`, unscoped.** Not on the critical path either way: gitops-api vendors the
   built ESM and `go:embed`s it, so npm is distribution, not a build dependency.

**Next:** Phase 2, upstream. Nothing in gitops-api changes until Phase 4.
