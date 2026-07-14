# Proposal 0002 — the real-cluster rung

**Status:** **B and A have landed.** C and D remain.
**Goal:** stop guessing. Every rung below this one fakes the API server, and *everything the gateway
believes about Kubernetes is currently unverified.*

> ## What the cluster actually said, and what it changed
>
> **Phase B — the fact-finder** (`task cluster-facts`) → [observed-v1.36.2+k3s1.md](../facts/observed-v1.36.2+k3s1.md).
> F1 holds: a streaming list really does end with a bookmark carrying `k8s.io/initial-events-end`, an
> annotation that appears nowhere in the API docs and on which the whole snapshot boundary rests. F3,
> F4, F5 and F7 hold as written.
>
> **F6 changed the design, and it is the reason this rung exists.** Pointed at a real aggregated API
> (Kubernetes' own sample-apiserver), the §3a streaming list was **refused outright** —
> `sendInitialEvents is forbidden ... unless the WatchList feature gate is enabled`. An aggregated API
> server is a separate binary with its own feature gates. So §3b's list-then-watch is **not** a
> fallback for old clusters: it is the *only* way to serve an aggregated API on a current one, and a
> gateway implementing only §3a cannot open a stream for a `Flunder` at all. That is a bug a user
> would have found, in their cluster, and we would not.
>
> **Phase A — the backend** (`gateway/kube`, its own module, so `go get .../gateway` stays
> dependency-free). It therefore ships **both** paths, and *detects* which to use — nobody should have
> to know which of their APIs is aggregated in order to watch it. `task test-cluster` proves both
> against the real thing: it first asserts the aggregated API still **refuses** §3a (so a cluster that
> quietly gained `WatchList` cannot make the test pass for the wrong reason), then asserts the gateway
> serves the scope anyway.
>
> Two notes for whoever does C:
>
> - The order in this proposal was **B before A**, and it paid for itself exactly as argued: F6 landed
>   *before* `gateway/kube` was written around an assumption that turned out to be false.
> - A scratch namespace deletes **asynchronously**. Two tests sharing one namespace name race the
>   first one's teardown ("because it is being terminated"). One namespace per test.

## Why this rung, and what it is NOT for

It is **not** for re-testing the merge, the framing or the projection. Those are settled by fixtures in
microseconds, deterministically, and re-running them against a cluster would only make them slower and
flakier.

It is for the handful of facts that a fake watch **cannot** establish, because a fake watch is a thing
I wrote and it will happily agree with me:

| # | the open question | why it matters | today |
|---|---|---|---|
| **F1** | Does a streaming list's terminating bookmark really carry `metadata.annotations["k8s.io/initial-events-end"] = "true"`? | **This is the snapshot boundary.** Get it wrong and `synced` fires at the wrong moment — or never, and the browser never paints. It is the single load-bearing assumption in the gateway | **[unverified]** — the annotation is in `apimachinery` (KEP-3157) but appears **nowhere** in the API docs |
| **F2** | Is `WatchList` on by default on our target, and does `sendInitialEvents` behave as §3a assumes? | if not, the list-then-watch fallback (§3b) is the *primary* path, not the fallback | unverified |
| **F3** | How does a `410 Gone` actually arrive on an open watch? | the whole `RESYNC_REQUIRED` → new cycle recovery hangs off recognising it | unverified |
| **F4** | What do real `resourceVersion`s look like — decimal? how large? | we now **require** orderability (1.35 conformance) and refuse loudly otherwise. That is a bet | unverified |
| **F5** | Does a real controller move `status` the way `status-follow-live` claims — `spec` byte-identical? | it is the product thesis, and the demo | unverified |
| **F6** | What `resourceVersion` does an **aggregated API** (wardle `Flunder`) serve? | it decides whether `OrderingLenient` is a real need or a theoretical one | unverified |

Answering F1–F6 is worth more than any number of extra scenarios. **So the first deliverable is not a
test suite — it is a fact-finder that writes its answers into `docs/facts/`.**

---

## Phase A — the missing production code: a real backend

There is no client-go in this repo at all. `Backend`/`Watcher` (seams.go) exist precisely so a real one
can drop in, and this is where that gets paid off:

```go
// gateway/kube — the only file in the project that imports client-go.
type KubeBackend struct{ Client dynamic.Interface }

func (b *KubeBackend) Watch(ctx context.Context, scope gateway.Scope) (gateway.Watcher, error)
```

Two things it must do, and both are exactly the shape the fixtures already describe:

1. the **streaming list** (§3a): `SendInitialEvents=true`, `ResourceVersionMatch=NotOlderThan`,
   `AllowWatchBookmarks=true`, `ResourceVersion: ""`, and map the initial-events-end bookmark to
   `InitialEventsEnd: true` — **the fact F1 exists to verify**;
2. adapt client-go's **channel** watch to our **pull** `Watcher` (about ten lines, as promised).

> **Decision needed: a separate Go module.** `go get .../krm-stream/gateway` is currently
> **zero-dependency**. If the kube backend lives in the same module, every adopter pulls client-go and
> its transitive world. I propose **`gateway/kube` as its own module** (its own `go.mod`), so the
> protocol core stays dependency-free and the adapter is opt-in. It costs one `go.mod` and a line in
> the Taskfile.

## Phase B — the cluster, and the fact-finder

**The cluster.** k3d, which the devcontainer already ships, pinned to a **≥1.35** k3s:

```bash
task cluster-up      # k3d cluster create --image rancher/k3s:v1.36.1-k3s1 --wait
```

Two fidelity notes that are worth one line of config each:

- **k3s defaults to kine (SQLite), not etcd.** `resourceVersion` and compaction/`410` semantics are
  then kine's, not etcd's — which is precisely what F3 and F4 are about, so we should not verify them
  against a substitute. Use k3s's **embedded etcd** (`--cluster-init`) so the storage layer is the real
  one.
- We need **no** audit policy, no webhook, no host mounts — so our cluster script stays ~10 lines,
  unlike gitops-reverser's (which needs all three).

**The aggregated API** (for F6): the upstream sample-apiserver — `Flunder`, `wardle.example.com`, the
same one our `resourceversion-*` fixtures already model. It is an image plus an etcd sidecar plus an
`APIService`. **Copy the manifests from upstream Kubernetes, not from gitops-reverser** — the one-way
rule says this repo depends on nothing of ConfigButler's, and that includes its YAML. Note the image
tag must track the cluster's minor version.

**The fact-finder.** One small program, one command:

```bash
task cluster-facts   # prints, asserts, and writes docs/facts/observed-<k8s-version>.md
```

For each of F1–F6 it opens a real watch, records what actually arrives, and **writes the answer into
`docs/facts/`** next to the documentation-derived claims — so the repo's facts are stamped with a
cluster version and a date, and the `[unverified]` flags either clear or turn into bugs. If F1 is
false, we find out in a paragraph rather than in a UI that never paints.

## Phase C — the fixtures, replayed for real

Then, and only then, the corpus: each gateway fixture's `watch:` ops driven as **real API operations**.

```
list      → create the objects, open the stream
added     → create
modified  → update
deleted   → delete
relist    → open a watch at a stale resourceVersion → the API server returns 410 Gone
disconnect→ kill the SSE connection
```

**Assert convergence, not bytes.** This is the one design rule of this rung. `resourceVersion`s,
timestamps, `managedFields` and coalescing are all nondeterministic, so byte-for-byte equality is the
*wrong* assertion here — it would be flaky by construction. The right one is what the protocol actually
promises: **the store ends up holding what the fixture says it holds** (`uids`, `dirty`, `conflicts`,
`draftSubset`, `patch`). We already have that assertion, in `test/expect.ts`.

**What does not run here.** Three ops cannot be provoked on demand against a real API
server, and pretending otherwise is how a suite becomes a liar:

| fixture | why it stays fake-watch-only |
|---|---|
| `bookmark-absorbed` | you cannot make the API server send a routine bookmark *when you want one* — the docs say so explicitly ("you shouldn't assume bookmarks are returned at any specific interval, nor… that the API server will send any") |
| `partial-object-refused` | requires a client to have negotiated a metadata-only watch; our gateway never does |
| `tombstone-without-uid` | `DeletedFinalStateUnknown` is an informer-internal race, not something you can ask for |
| `resourceversion-unorderable` | needs a non-conformant upstream. F6 tells us whether one even exists |

That is not a gap — it is exactly *why* the fake-watch rung exists, and it is the strongest argument I
have for keeping both.

## Phase D — the demo, for real

The payoff, and it is nearly free once A–C land: point `examples/vanilla-browser` at a gateway watching
a **real Deployment**, then `kubectl scale` it in another terminal and watch `status` climb in the
browser while you edit `spec`. Same page, same store, real cluster.

```bash
task demo-cluster
```

---

## Cost, and what runs where

| phase | what | rough size |
|---|---|---|
| A | `gateway/kube` (streaming list + pull adapter + its own go.mod) | ~200 lines |
| B | `task cluster-up` + sample-apiserver manifests + the fact-finder | ~250 lines, mostly YAML |
| C | fixture replay against the cluster, `//go:build e2e` | ~200 lines |
| D | `task demo-cluster` | ~20 lines |

**CI:** this rung does **not** run per-PR. It needs Docker-in-Docker and it takes minutes, and the
fixtures already gate every PR in milliseconds. It runs **nightly and on a label** — and, critically,
**the fact-finder runs on the cluster's version bump**, because F1–F6 are answers about *a version of
Kubernetes*, not about our code.

## The order I would do it in

**B before A.** The fact-finder (F1–F4) needs a watch, not a *conforming gateway* — a 50-line program
against client-go answers the load-bearing questions before we build the backend that assumes them.
Finding out that F1 is false after writing `gateway/kube` around it would be the expensive way round.

So: **B (facts) → A (backend) → C (fixtures) → D (demo).**
