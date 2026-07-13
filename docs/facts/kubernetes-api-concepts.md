# Facts: the Kubernetes API, as Kubernetes actually documents it

> **Source:** <https://kubernetes.io/docs/reference/using-api/api-concepts/>, read in full from the
> upstream markdown (`kubernetes/website`, `content/en/docs/reference/using-api/api-concepts.md`) on
> **2026-07-11**. This file is a summary *with citations to that page*, plus — kept strictly
> separate — what each fact means for this repo.
>
> **Why this file exists.** `spec/v1.md` and `gateway/README.md` make claims about what a Kubernetes
> watch does. Those claims were written from memory. Two of them were **wrong**, and one of the two
> was a live bug in the gateway (§2). Design decisions here get grounded in what the API server
> documents, or they do not get made.
>
> **What is NOT in this file:** anything the page does not say. Where we rely on behaviour that lives
> in `client-go`/`apimachinery` rather than in the documentation, it is called out as **[unverified
> against docs]** and is a job for the real-cluster suite.

---

## 1. The watch event vocabulary

The page documents a watch as a stream of change notifications, each a JSON document of the form
`{"type": …, "object": …}`, and names these types:

| type | in the docs |
|---|---|
| `ADDED` | yes — including **synthetic** ADDEDs that establish initial state |
| `MODIFIED` | yes |
| `DELETED` | yes (as an operation; the event type appears in the examples) |
| `BOOKMARK` | yes — a whole section, [Watch bookmarks](https://kubernetes.io/docs/reference/using-api/api-concepts/#watch-bookmarks) |

**`ERROR` is not enumerated on this page.** It exists in `k8s.io/apimachinery`'s `watch.Event`
(`watch.Error`, carrying a `metav1.Status`), and it is how a `410 Gone` reaches a watch that is
already open. **[unverified against docs]**

### 1.1 A BOOKMARK's object is a PARTIAL OBJECT. This is the headline.

> "It is a special kind of event to mark that all changes up to a given `resourceVersion` the client
> is requesting have already been sent. The document representing the `BOOKMARK` event is of the type
> requested by the request, **but only includes a `.metadata.resourceVersion` field**."

Their example, verbatim:

```json
{ "type": "BOOKMARK",
  "object": {"kind": "Pod", "apiVersion": "v1", "metadata": {"resourceVersion": "12746"} } }
```

So an object with **no `uid`, no `name`, no `spec`, no `status`** is not a pathological edge case
someone might contrive — **it is on every conforming watch stream that asked for bookmarks.** A
gateway that forwards a BOOKMARK's object as `added`/`modified` hands its consumer a fragment; a
consumer whose model is "replace, never merge" (ours, and correctly so) then blanks the resource on
screen.

Two further rules from the same section:

- Bookmarks are **opt-in**: `allowWatchBookmarks=true` on the watch request.
- "You **shouldn't assume bookmarks are returned at any specific interval**, nor can clients assume
  that the API server will send any `BOOKMARK` event even when requested." → nothing may be built on
  a bookmark *arriving*. It is a hint, not a heartbeat.

## 2. Resource versions — and the bug this page found in our gateway

> "Resource version strings are **orderable as monotonically increasing integers within the same
> resource type**… Both resource versions must be from objects of the same API group and resource
> type."

So far so good — that is what `spec/v1.md` §6 assumes. But then:

> "Both must start with a digit 1-9 and contain only digits 0-9. **Resource versions are compared as
> arbitrary bitsize decimal integers**… **The bitsize must not be assumed to be some fixed amount.**"

And the page's own worked example is a **40-digit** resource version:

> `"2345678901234567890123456789012345678901" > "345678901234567890123456789012345678901"`

The prescribed comparison is **lexicographic-by-length**:

> "If they are not of equal length, the longer one is greater (for example, "123" > "23"). If they
> are of equal length, the lexicographically greater one is greater."

**We were parsing `resourceVersion` with `strconv.ParseInt` (int64 — 19 digits).** A 40-digit
resource version overflows it, `ParseInt` fails, and our staleness check silently gives up — or
worse, on a value that *does* parse but has wrapped, compares nonsense. The failure mode is
**dropped live updates**, which in a status-watch UI looks exactly like "Kubernetes is slow."

And the case that has no integer at all:

> "If you are using API resources served by an **extension API server**… If either of two resource
> version strings does not parse as a decimal number, the two strings can be checked for **equality**
> but you **cannot** rely on comparisons for ordering."

→ For a non-numeric `resourceVersion`, **ordering is undefined** and the only safe thing to do is not
order. Not "guess". Not "fall back to string compare".

### 2.1 …but from 1.35, orderability is a **conformance requirement**

This is the sentence that decides the design, and it is stronger than the caveat above:

> "Starting with Kubernetes 1.35, orderability of resource versions for all Kubernetes types is
> included in **Certified Kubernetes requirements**. Base API objects **and custom resources** **must**
> be orderable as a monotonically increasing integer for any 1.35+ APIServer implementation in order to
> pass conformance tests."

So on a supported cluster, an unorderable `resourceVersion` **cannot occur** — not for built-ins, not
for CRDs. The "may not parse as a decimal" escape is scoped to **extension / aggregated API servers**,
which are third-party implementations that this conformance test does not cover.

That makes "can I trust `resourceVersion` to increase?" a real decision rather than a shrug, and this
library takes the strong side: **it REQUIRES Kubernetes 1.35+, trusts orderability, and refuses loudly
when the upstream lies** (`Gateway.Ordering = OrderingStrict`, the default), with an explicit
`OrderingLenient` for an aggregated API. Degrading *silently* on every cluster in order to accommodate
one is the wrong trade: a consumer that was promised per-object monotonicity and is quietly no longer
getting it is worse off than one that has been told.

**Where you actually meet an unorderable resourceVersion — and where you do not.** This distinction is
now baked into the corpus, because getting it wrong means writing a fixture for a scenario that cannot
occur:

| server | orderable? | why |
|---|---|---|
| kube-apiserver, built-in types | **yes**, guaranteed | 1.35 conformance |
| kube-apiserver, **CRDs** | **yes**, guaranteed | 1.35 conformance says "base API objects **and custom resources**" |
| **aggregated / extension** API server | **not guaranteed** | a third-party implementation; the conformance test does not cover it. This is the *only* case the docs' equality-only carve-out is written for |

And a related fact worth stating, because it decides the *other* fixture: **kube-apiserver's
`resourceVersion` is an etcd revision** — an int64, at most 19 digits. The docs' 40-digit example
therefore cannot have come from kube-apiserver; it is a value only a server with a different backing
store produces. Both facts point the same way, so both `resourceversion-*` fixtures use a **`Flunder`**
(`wardle.example.com/v1alpha1`, Kubernetes' own [sample-apiserver](https://github.com/kubernetes/sample-apiserver))
rather than a ConfigMap.

Also, and we get this right already:

- **Opaque to clients.** "Resource versions must be passed unmodified back to the server."
- Comparison is only valid **within one resource type**. A Pod's RV and a Deployment's are not
  comparable. (Our high-water map is keyed by uid within one scope — one GVR — so this holds.)
- On a **list**, `.metadata.resourceVersion` of the *collection* is the version the collection was
  constructed at — which is **not** related to the `.metadata.resourceVersion` of the items in it.

## 3. Streaming lists (`sendInitialEvents`) — the snapshot boundary

> "the initial state can be requested by specifying `sendInitialEvents=true`… the API server starts
> the watch stream with synthetic init events (of type `ADDED`) to build the whole state of all
> existing objects **followed by a `BOOKMARK` event (if requested via `allowWatchBookmarks=true`)**.
> The bookmark event includes the resource version to which is synced. After sending the bookmark
> event, the API server continues as for any other watch request."

Preconditions, stated as requirements:

- `sendInitialEvents=true` **requires** `resourceVersionMatch=NotOlderThan`.
- `resourceVersion` empty or absent ⇒ a **consistent read**; the bookmark is sent once the state is
  synced at least to the moment the request began being processed.
- `allowWatchBookmarks=true` is what makes the terminating bookmark appear at all.

This maps **one-to-one** onto our protocol: the synthetic ADDEDs are the snapshot, the terminating
bookmark **is** `synced`, everything after it is live. That is exactly what `gateway/README.md` §3a
claims, and it checks out.

**One claim of ours the page does NOT support — now settled against a real cluster.**
`gateway/README.md` §3a says the terminating bookmark is identified by
`metadata.annotations["k8s.io/initial-events-end"] == "true"`. **That annotation appears nowhere on
this page.** It is `metav1.InitialEventsAnnotationKey` in `apimachinery` (KEP-3157), and it was the
single load-bearing assumption in the whole gateway — if it were wrong, `synced` would fire at the
wrong moment, or never, and the browser would never paint.

> ✅ **CONFIRMED** on Kubernetes **v1.36.2** — see [observed-v1.36.2+k3s1.md](observed-v1.36.2+k3s1.md),
> F1. The bookmark arrives, and it carries `k8s.io/initial-events-end: "true"`.
>
> And a detail the docs get *slightly* wrong, which we only know because we looked: the page says a
> bookmark's object "only includes a `.metadata.resourceVersion` field", but the real terminating
> bookmark also carries `metadata.annotations` (it has to — that is where the marker lives). What it
> does **not** carry is a `uid`, which is what the gateway's partial-object guard actually keys on.
> The guard is correct, but for a reason one shade more precise than the sentence it was written from.

Note also: with `resourceVersion` **unset** (no `sendInitialEvents` at all), a watch is "Get State and
Start at Most Recent" and *also* "begins with synthetic 'Added' events for all resource instances that
exist at the starting resource version" — but with **no terminating bookmark**, so you cannot tell
where the snapshot ends. That is the whole reason `sendInitialEvents` + `allowWatchBookmarks` exist,
and the whole reason a list-then-watch fallback (§3b) has to synthesize the boundary itself.

## 4. Losing continuity: `410 Gone`

> "A given Kubernetes server will only preserve a historical record of changes for a limited time.
> **Clusters using etcd 3 preserve changes in the last 5 minutes by default.** When the requested
> watch operations fail because the historical version of that resource is not available, clients
> must handle the case by recognizing the status code `410 Gone`, **clearing their local cache,
> performing a new get or list operation, and starting the watch from the `resourceVersion` that was
> returned**."

"Clear the cache, re-list, restart the watch" **is** our `reset` … `synced` cycle. The five-minute
window is why `resync-midstream` is a fixture and not a curiosity: a browser tab left open on a quiet
namespace, behind a laptop lid, will hit this routinely.

Also: `resourceVersion="0"` on a watch means "Get State and Start at **Any**", which the page warns
"may return **arbitrarily stale** data" and can **rewind** to a version the client already observed.
→ **Never open our watches with `resourceVersion=0`.** Per-object monotonicity would be violated by
the upstream itself, and our own high-water map would then (correctly, but uselessly) drop half the
snapshot.

## 5. Deletion is two-phase, and the UI must show it

> "When a client first sends a **delete**… the `.metadata.deletionTimestamp` is set to the current
> time. Once the `.metadata.deletionTimestamp` is set, external controllers that act on finalizers may
> start performing their cleanup work… **Once the last finalizer is removed, the resource is actually
> removed from etcd.**"

So a delete of a finalized object appears on the watch as **`MODIFIED` (now carrying
`.metadata.deletionTimestamp` and `.metadata.finalizers`) — possibly for a long time — and only later
as `DELETED`.** An object that is "Terminating" is a first-class, observable state, and it arrives
through the ordinary upsert path. A live status view that does not surface it is lying about the
cluster.

The page says nothing about whether a `DELETED` event's object is complete, and nothing about
informer deletion tombstones (`cache.DeletedFinalStateUnknown`) — that is a `client-go` construct.
**[unverified against docs]**

## 6. Partial objects are a first-class, client-requested thing

> "To request partial object metadata, you can request metadata only responses in the `Accept`
> header… `Accept: application/json;as=PartialObjectMetadata;g=meta.k8s.io;v=v1`… **the returned
> objects only contain the `metadata` field. The `spec` and `status` fields are omitted.**"

The returned objects have `kind: PartialObjectMetadata`, `apiVersion: meta.k8s.io/v1` — and a full
`metadata`, *including a `uid`*. This matters for us: **a "partial object" is not always detectable by
a missing uid.** A `PartialObjectMetadata` has one. The reliable check is the **kind**, and the
consequence of missing it is that a consumer replaces a real object with one that has no `spec` and no
`status` — the status view goes blank and the editor loses the user's `spec`.

(An aggregated API server may not support partial fetches at all and returns `406`.)

## 7. Writes

Four PATCH content types, and the page names them exactly:

| `Content-Type` | what |
|---|---|
| `application/apply-patch+yaml` | Server-Side Apply (create-or-patch) |
| `application/json-patch+json` | RFC 6902 |
| **`application/merge-patch+json`** | **RFC 7386** ← what `patch(id)` builds |
| `application/strategic-merge-patch+json` | k8s-specific; **not usable with CRDs** |

- **PUT** requires the client to send `resourceVersion`; a stale one gets **`409 Conflict`**. The page
  independently warns that a PUT "might accidentally drop fields… you could receive fields that your
  client does not know how to handle - and then drop them as part of your update" — which is the exact
  argument `spec/v1.md` §3 makes for never round-tripping a projected object. Good: the protocol's
  most opinionated rule is one Kubernetes itself makes.
- A PATCH may *also* carry `resourceVersion` as a precondition against lost updates. Our `patch(id)`
  does not, today — a deliberate gap worth revisiting, since we *have* the resourceVersion and the
  user has been staring at a three-way merge.
- Strategic Merge Patch "has been superseded by Server-Side Apply", and **cannot** be used with CRDs.
  → the keyed-list merge we want (docs §4.1, `x-kubernetes-list-map-keys`) points at **SSA**, not SMP.

`x-kubernetes-list-type` / `x-kubernetes-list-map-keys` are **not on this page** (they live in the
Server-Side Apply reference). The array-merge open question needs that page read the same way this one
was, before it gets decided.

---

## What this changes in this repo

| # | Fact | Consequence | Status |
|---|---|---|---|
| 1 | RVs are arbitrary-bitsize integers; a 40-digit one is legal | `strconv.ParseInt` in `isStale` **overflows** → silently drops live updates | **fixed** — string compare, length-then-lexicographic |
| 2 | **1.35+ requires orderability** (built-ins *and* CRDs) | trust it: `OrderingStrict` is the default, and an unorderable RV is a **terminal error naming the fix**, not a silent degradation | **done** — fixture `resourceversion-unorderable` |
| 2b | Aggregated/extension API servers are **not** covered by that conformance test | they need an escape hatch: `OrderingLenient` orders nothing it cannot order, and **drops nothing** | **done** |
| 3 | A BOOKMARK's object has only `.metadata.resourceVersion` | a partial object is on **every** conforming stream; forwarding it blanks the consumer | **fixed** + fixture `bookmark-absorbed` |
| 4 | `PartialObjectMetadata` has a **uid** | "no uid" is not a sufficient partial-object check; check the **kind** | **fixed** + fixture `partial-object-refused` |
| 5 | Bookmarks may never arrive | nothing may depend on a bookmark's *arrival*, only on its meaning | holds — we only use the terminating one |
| 6 | `k8s.io/initial-events-end` is not in the docs | our snapshot boundary rests on it | ✅ **CONFIRMED on v1.36.2** — [observed](observed-v1.36.2+k3s1.md) F1 |
| 6b | **An aggregated API REJECTS `sendInitialEvents`** ("forbidden … unless the WatchList feature gate is enabled") | the streaming list is **not universal**. §3b's list-then-watch is not a fallback for old clusters — it is **mandatory**, today, for aggregated APIs | ❗ **open** — [observed](observed-v1.36.2+k3s1.md) F6. The gateway cannot serve a `Flunder` until it has this path |
| 6c | An aggregated API has **its own** resourceVersion space, starting at ~1, and its store may be ephemeral | RVs can go **backwards** across a restart of that API server | ✅ survived — the high-water map is per-**cycle**, so a restart's new cycle clears it. A per-stream map would have swallowed every event after a restart |
| 7 | Delete is two-phase; `deletionTimestamp` arrives as `MODIFIED` | "Terminating" is an observable state we must render, not a gap | **open** — no fixture yet |
| 8 | `rv=0` watches may rewind and serve stale data | never open a watch with `resourceVersion=0` | holds — §3a uses `rv=""` |
| 9 | RFC 7386 merge patch is a real k8s content type | `patch(id)` is directly usable, `application/merge-patch+json` | holds |
| 10 | PATCH may carry `resourceVersion` as a lost-update precondition | we could, and probably should, offer it | **open** |
| 11 | SMP is superseded and CRD-incompatible | keyed-list merge must go via **SSA**, not strategic merge | **open** (docs §4.1) |
