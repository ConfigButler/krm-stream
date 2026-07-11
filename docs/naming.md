# Naming the KRM streaming family

> **Status: DECIDED (2026-07-11).** `krm-stream`, **unscoped** on npm. The repo is live:
> [ConfigButler/krm-stream](https://github.com/ConfigButler/krm-stream), public, seeded, CI green.
> The reasoning is kept below — *why* a name was rejected is the part that stops someone
> re-proposing it in three months. Feeds decision #1 of the
> [monorepo plan](extraction-plan.md). The family is three artifacts —
> [protocol](../spec/v1.md), [engine](client-state-model.md),
> [gateway](../gateway/README.md) — that need one umbrella name and three package names.
>
> **Recommendation: `krm-stream`.** This document previously recommended `krm-live`; a review found a
> collision that kills it (§4), and the argument that replaced it is better than the one it replaced.
> The reasoning is kept below rather than quietly rewritten, because *why* a name was rejected is the
> part that stops someone re-proposing it in three months.

---

## 1. Who is this name actually for?

The most useful thing this document can say is who it **isn't** for.

### Audience A — the managed ConfigButler customer *(never sees the name)*

They signed up to edit their config. They do not know they are talking to a Kubernetes API server,
they do not know what a ConfigMap is, and they will never type `npm install` or read a `go.mod`. To
them the product surface says *"Your config"*, *"Save"*, *"changed in the cluster"* — and it must keep
saying exactly that.

**Consequence:** they impose **zero constraints on the library name**, and one strong constraint on
the *product* vocabulary — no Kubernetes nouns leak into their UI. Those are two different vocabularies
and it is liberating to separate them. The library is free to be as bluntly Kubernetes-native as it
likes, precisely *because* the person who doesn't want to know never reads it.

This cuts the naming problem in half. Everything below is for audiences B and C.

### Audience B — the platform engineer running their own operator

They have a cluster, they have CRDs, and they want a live window onto them in a browser without
writing the watch/SSE/merge machinery themselves. They find us by **searching for the problem**, and
they decide in about five seconds. They are typing things like:

> `kubernetes watch stream browser sse` · `live kubernetes resource editor react` ·
> `three-way merge kubernetes object frontend` · `krm resource stream`

They are not charmed by cleverness. They want a name that *asserts the thing does what they just
typed*. A name that needs a blog post to explain has already lost them.

### Audience C — the AI writing their frontend *(the new one, and the one that changes the answer)*

This is the audience Simon spotted, and it genuinely inverts a rule that held for the last decade.

A model recommends a library by **mapping a concept to a name it has seen co-occur with that
concept**. It cannot browse a beautiful landing page. Which means:

- **A literal name is retrievable; a metaphor is not.** Asked "what streams live Kubernetes objects to
  a browser?", a model can *reconstruct* `krm-stream` from meaning alone — the name is a compressed
  description, and "stream" is a word already in the question. It cannot reconstruct `Porthole`,
  `Loupe`, or `Sextant` from anything at all. A metaphor is a name you can only recall if you already
  know it, which is exactly the thing an agent doesn't.
- **A colliding name gets pulled toward the incumbent** — and the incumbent wins. This is what kills
  both `kube-live` and `krm-live` (§4). Hallucinated-adjacent is worse than unknown.
- **A scope hurts recall.** An agent that half-remembers `krm-stream` runs `npm i krm-stream` and
  succeeds. One that half-remembers `@configbutler/krm-stream` and misspells the org gets a 404.
  **Unscoped names are more retrievable than scoped ones** — and this is the first time that has ever
  been an argument worth making. (This is the one point where I still disagree with the review; §6.)
- **The name should be the search term.** Our docs, the spec, the fixtures and the error strings will
  all contain the phrase; that co-occurrence is what a model trains and greps on. A literal name
  compounds. A metaphor has to be taught.

**The test to apply to any candidate:** *could a model that has never heard of us produce this name
from a description of the problem?* If yes, the name is doing retrieval work for free.

---

## 2. What the name has to do

| # | Criterion | Why |
|---|---|---|
| 1 | **Be the search query** | audiences B and C both arrive by describing the problem |
| 2 | **Say KRM, honestly** | the pitch is that it streams *real Kubernetes objects*, not abstracted "documents". A name that hides that is selling a different, worse product |
| 3 | **Not collide** | mis-recall toward a stronger incumbent is worse than obscurity |
| 4 | **Name the shared foundation** | one umbrella over a protocol, a producer and a consumer. The umbrella should name **what they share**, not what one of them does |
| 5 | **Stand alone from ConfigButler** | a stranger must be able to adopt it without adopting us. It is not `configbutler-console` |
| 6 | **Leave room to grow** | React bindings, a second gateway backend, test tooling — none of which should force a rename |
| 7 | **Survive the trademark question** | the Linux Foundation restricts "Kubernetes"/"K8s" in *product* names. `kube-` is common practice and low-risk; `KRM` sidesteps it entirely |

Note what is **not** on this list: memorability, charm, and a nautical pun. Those are branding criteria
for a product with a marketing surface. This is a library whose two real audiences both arrive via a
search box — and one of them isn't human.

---

## 3. The candidates

### The literal family

| Name | For | Against |
|---|---|---|
| **`krm-stream`** ✅ | names **the shared contract**, which is the only reason the monorepo exists (plan §1). "Stream" is a word already in the user's query. Free everywhere. Broad enough to hold React bindings, a second backend, conformance tooling — none of which are "live editing" | "stream" alone undersells the three-way merge — the tagline has to carry it (§5) |
| ~~`krm-live`~~ ❌ | *(previously recommended)* | **collides with `kpt live`** (§4). Fatal |
| ~~`kube-live`~~ ❌ | higher search volume | collides with [kubelive](https://github.com/ameerthehacker/kubelive), a popular kubectl TUI |
| `kubestream` / `kube-resource-stream` | legible without knowing "KRM" | loses the KRM framing that is our whole positioning; the longer one is a mouthful in every import |
| `resource-stream` | generic | no Kubernetes signal at all → unfindable, and it means nothing to a model. Fails criteria 1 and 2 |
| `krm-sync` | — | rejected: **the wire protocol is deliberately one-way**. "Sync" promises bidirectional replication we do not implement. Naming a lie is expensive later |
| `krm-reconciler` | — | rejected: as a *public package* name it reads as a Kubernetes controller. It is a browser state library. ("Live reconcile engine" is fine as an internal architectural description; it is not fine on npm) |

### The evocative family (and why they lose)

`Porthole` (a window in a hull onto the live sea — and it lands squarely in Kubernetes' nautical
tradition: helm, harbor, rudder), `Loupe`, `Sextant`, `Crow's Nest`, `Vantage`.

`Porthole` is the one I actually like, and it is the one I argued against. It is a genuinely good
*metaphor* — an honest live window onto something moving, which is precisely the product's thesis. But
it fails every criterion that matters here: it cannot be reconstructed from the problem, it teaches an
agent nothing, and **it is already taken on npm**. It is a brand for a product with a landing page,
which this is not. Keep it in the drawer for a hosted product; don't spend it on a library.

### Availability (checked, 2026-07-11)

| Name | npm | GitHub |
|---|---|---|
| **`krm-stream`** | **free** (scoped and unscoped) | **no repos at all** |
| `krm-live` | free | no repos — *but see §4; availability was never the problem* |
| `kubelive` | taken | taken (popular kubectl TUI) |
| `porthole`, `loupe`, `sextant`, `krm` | taken | — |

---

## 4. Why `krm-live` is dead (the finding that changed this document)

`kpt live` is established terminology for applying, pruning, observing and reconciling KRM packages.
This is not a distant-namespace coincidence:

- [kptdev/kpt](https://github.com/kptdev/kpt) — **1.9k stars**, and its own one-line description is
  *"Automate Kubernetes Configuration Editing."* That is a paraphrase of our product.
- `live` is a **top-level command** in that repo (`kpt live apply`, `kpt live status`, …).

So the collision is not adjacent — it is **the same domain, the same vocabulary, and the same
audience**. Worse, it is targeted precisely at the people who *know what KRM means*: the kpt /
Config Sync / config-as-data community is the one crowd guaranteed to read `krm-live` and think "the
kpt live thing." We would spend the project's life being asked what our relationship to kpt is, and
audience C — trained on far more kpt text than ours — would answer that question wrong on our behalf.

I missed this. The review that caught it is right, and it is a better argument than any I made for the
name it replaces.

---

## 5. `live` doesn't die — it moves to where it's true

The one real cost of `krm-stream` is that "stream" describes the read path, and half this project's
value is the three-way merge and the conflict model on top of it. The fix is not to name the project
after the capability; it is to **put the capability where a developer actually meets it** — in the API
and in the first sentence of the README.

```js
import { LiveResourceStore, connectResourceStream } from "krm-stream";
```

`LiveResourceStore` is the central object: it tells a frontend developer what they get (a live store of
resources) without implying it runs a controller loop. The project is the stream; *live* is what the
store is.

**Tagline, under the name in the README:**

> **`krm-stream` — a live, honest window onto Kubernetes resources, in the browser.** A language-neutral
> protocol for streaming Kubernetes Resource Model objects, a Go gateway that produces the stream, and
> a JavaScript library that reconciles it against local edits. Watch `status` reconcile in real time;
> edit `spec` with a real three-way merge; real RBAC, real attribution.

That sentence is what "KRM" doesn't say on its own, and it is the sentence that gets indexed.

---

## 6. The naming family

| Thing | Name |
|---|---|
| project | **KRM Stream** |
| repo | `github.com/ConfigButler/krm-stream` |
| protocol | **KRM Resource Stream Protocol**, v1 — a spec, not a package. Endpoint stays `…/resource-stream/v1` |
| JS package | npm **`krm-stream`** — *unscoped* (see below) |
| central JS type | `LiveResourceStore` |
| Go module | `github.com/ConfigButler/krm-stream/gateway`, tagged `gateway/vX.Y.Z` |
| Go component | **KRM Stream Gateway** |
| Go binary *(later — see §7)* | `krm-stream-gateway` |
| container *(later — see §7)* | `ghcr.io/configbutler/krm-stream-gateway` |

**Keep "gateway" as a component name, never the project name.** "Gateway" is heavily loaded in
Kubernetes (Gateway API and its many implementations); unprefixed, `resource-stream-gateway` is both
generic and confusable. Prefixed — `krm-stream-gateway` — it is unambiguous. Agreed with the review,
adopted.

**The npm scope: decided — unscoped.** The review proposed `@configbutler/krm-stream`. We are shipping
plain **`krm-stream`** (it was free; `packages/krm-stream/package.json` now claims it). A scope signals
provenance and blocks squatting, both real — but it costs recall with the audience most likely to type
the name from memory, and that audience is now a machine that will `npm i krm-stream` and get a 404 if
it misremembers the org. If squatting ever becomes a worry, `@configbutler/krm-stream` can be published
later as a thin re-export; the reverse is not possible.

Nothing on the critical path depends on publishing at all: gitops-api vendors the built ESM and
`go:embed`s it (Phase 3 of the plan), so npm is a distribution decision, not a build dependency.

---

## 7. What this changes in the monorepo plan

The rename is cheap. Three substantive things ride along with it:

1. **The repo layout gets better, and it fixes a real gap I flagged.** The reviewed layout splits
   `conformance/` into `fixtures/` + `gateway/` + `client/` — which is exactly the harness separation
   the plan was missing (it assumed one fixture set both suites could "load", which is impossible: a
   gateway needs *watch ops in → events out*, a client needs *events + edits in → draft out*). Adopt
   it, and design the fixture schema in three parts: `watchOps` (gateway input), `events` (**the shared
   surface — the only genuinely shared part**), `client` (edits + expected draft/dirty/conflict/patch).

   ```
   krm-stream/
     README.md  LICENSE
     spec/         v1.md  events.schema.json  examples/
     gateway/      go.mod  cmd/krm-stream-gateway/  internal/  README.md
     packages/
       krm-stream/ package.json  src/  README.md
     conformance/  fixtures/  gateway/  client/
     examples/     vanilla-browser/  react/
   ```

2. **Doc renames on the way in:** `resource-stream-protocol.md` → `spec/v1.md`;
   `resource-stream-gateway.md` → `gateway/README.md`; `live-reconcile-engine.md` →
   `docs/client-state-model.md` (the *engine* framing is right internally and wrong publicly — see the
   `krm-reconciler` rejection above).

3. **The binary and the container are a scope expansion — take the names, defer the artifacts.**
   gitops-api consumes the gateway as a **Go library**, not as a deployed service; a standalone
   `krm-stream-gateway` binary + `ghcr.io` image is a *new* deliverable with its own config, auth and
   release story. The names are right and worth reserving now. Build the binary when the
   `examples/` demo needs it (plan Phase 6), not before — it is not on the path to the console.

---

## 8. Decision — settled 2026-07-11

```
Project:     KRM Stream
Repository:  ConfigButler/krm-stream   (public, seeded, CI green)
Protocol:    KRM Resource Stream Protocol, v1
JS package:  krm-stream                (UNSCOPED — §6)
JS concept:  LiveResourceStore
Go module:   github.com/ConfigButler/krm-stream/gateway
Go service:  krm-stream-gateway        (names reserved; artifact deferred — §7.3)
```

Nothing here is open any more. The tagline in §5 is what carries the half that "stream" undersells —
if the editor story ever feels invisible, that sentence is the thing to fix, not the name.
