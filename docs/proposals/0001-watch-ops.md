# Proposal 0001 — three watch ops the corpus cannot currently say

**Status:** accepted, implemented in this commit.
**Affects:** `conformance/` (fixture format + new fixtures), `gateway/` (one real bug), `spec/v1.md`
(one non-normative claim that was wrong). **Not** a wire change: no event type, field or framing rule
changes, so no `/v2`.

## The problem, and how it was found

The gateway passes all 11 of its conformance fixtures. Then I mutated it, to check the corpus could
actually go red:

| mutation | fixtures failing |
|---|---|
| emit `synced` on **every** bookmark, not just the snapshot's | **0** |
| forward a **partial object** (no uid) as `added`/`modified` | **0** |

Both are `MUST NOT`s — [spec §2](../../spec/v1.md), and rows 7–8 of the gateway's own failure matrix.
Both would break a browser. And the corpus is **structurally incapable of catching either**, because a
fixture's `watch:` script has exactly six ops — `list`, `added`, `modified`, `deleted`, `relist`,
`disconnect` — and none of them can say *"the API server sent a bare BOOKMARK"* or *"the informer
handed us a tombstone with no uid"*.

A corpus that cannot express a rule is not defending it. That is the whole point of this repository,
so the corpus grows three ops.

## Why these three, and not three others

Every op below is a thing **Kubernetes documents itself as doing** — see
[docs/facts/kubernetes-api-concepts.md](../facts/kubernetes-api-concepts.md), which is a read of the
[API concepts page](https://kubernetes.io/docs/reference/using-api/api-concepts/) rather than a read of
my own memory. That distinction produced two corrections to this repo on its own (below), so it is not
a formality.

### `bookmark` — a routine watch bookmark

Kubernetes, verbatim: *"The document representing the `BOOKMARK` event is of the type requested by the
request, **but only includes a `.metadata.resourceVersion` field**."*

```json
{ "type": "BOOKMARK",
  "object": {"kind": "Pod", "apiVersion": "v1", "metadata": {"resourceVersion": "12746"} } }
```

So this is not an exotic case: **an object with no uid, no name, no spec and no status is on every
conforming watch stream that asked for bookmarks** (`allowWatchBookmarks=true`, which the gateway must
set — it is how the snapshot boundary arrives at all).

The gateway must **absorb** it: never forward it as an upsert, and never mistake it for the snapshot
boundary. Only the bookmark that *terminates the initial events* is `synced`.

> ```yaml
> - { op: bookmark, resourceVersion: "12746" }
> ```

### `partial` — a metadata-only object delivered as an upsert

Kubernetes serves `PartialObjectMetadata` (`Accept: application/json;as=PartialObjectMetadata;…`):
*"the returned objects only contain the `metadata` field. The `spec` and `status` fields are omitted."*

This corrected a real mistake in our implementation. Our guard was **"an object with no uid is
partial"** — but a `PartialObjectMetadata` **has a uid**. It has a whole `metadata`. What it does not
have is a `spec` or a `status`. Forwarding one replaces a live Deployment with a husk: the status view
goes blank and the editor loses the user's `spec`. Our check was looking at the wrong field, and the
corpus could not tell us.

The honest check is the **kind**: `PartialObjectMetadata` / `PartialObjectMetadataList`, in group
`meta.k8s.io`. Plus the uid check, which still catches the bookmark's object.

> ```yaml
> - { op: partial, body: deploy-web.v1 }   # delivered as PartialObjectMetadata: metadata only
> ```

### `tombstone` — a deletion with no trustworthy identity

`client-go`'s informer delivers `cache.DeletedFinalStateUnknown` when it missed the delete and noticed
during a relist. **[Not in the API docs — a client-go construct, flagged as such in the facts file.]**
[spec §4.2](../../spec/v1.md) already says what to do, and says it in strong terms: if the gateway
cannot recover a trustworthy uid it **MUST NOT** emit an ambiguous `deleted` — it begins a new snapshot
cycle instead and lets `reset`…`synced` prune. A guessed uid deletes **the wrong object** out of
somebody's browser.

> ```yaml
> - { op: tombstone, body: cm-app.v1 }   # a DELETED whose object lost its identity
> ```

## The two corrections this exercise produced

**1. `resourceVersion` overflow — a live bug, dropping live updates.**

Kubernetes: *"Resource versions are compared as **arbitrary bitsize decimal integers**… The bitsize
must not be assumed to be some fixed amount."* Their own worked example is **40 digits**.

`gateway/stream.go` compared them with `strconv.ParseInt` — **int64, 19 digits**. On a cluster whose
resource versions exceed that, the parse fails and the monotonicity check gives up; the practical
result is **silently dropped live updates**, which in a status view is indistinguishable from "the
cluster is slow". Fixed: compare as strings, longer-is-greater then lexicographic, exactly as the docs
prescribe.

And the case with no integer at all: an **extension API server** may serve non-numeric resource
versions, where *"the two strings can be checked for equality but you cannot rely on comparisons for
ordering."* So an unorderable pair must never cause a drop. Fixed, and fixtured
(`resourceversion-bignum`).

**2. `spec/v1.md` §6 overclaimed.** Its non-normative note said a resource version "is a monotonically
increasing **integer**", which invites exactly the `ParseInt` that bit us. Corrected to say what
Kubernetes says: orderable as an arbitrary-precision decimal *within one resource type*, not orderable
at all if it is not decimal. The normative rule ("`resourceVersion` is opaque to consumers") is
unchanged — this only fixes the rationale that a *gateway* implementer reads.

## What does not change

- **No wire change.** No new event type, no new field, no framing change. A v1 consumer built against
  today's spec is unaffected: all three ops describe things the gateway must *absorb or refuse*, and
  the correct behaviour for every one of them is to emit **fewer** events, not different ones.
- **The client suite is untouched.** `watch:` is the gateway's input; the TypeScript loader already
  ignores it.

## New fixtures

| fixture | defends |
|---|---|
| `bookmark-absorbed` | a routine BOOKMARK is absorbed — never forwarded, never mistaken for `synced` |
| `partial-object-refused` | a `PartialObjectMetadata` upsert is refused; the gateway resnapshots rather than blanking the object |
| `tombstone-without-uid` | an ambiguous tombstone is never emitted as `deleted`; a new cycle prunes instead |
| `resourceversion-bignum` | a 40-digit resourceVersion orders correctly, and a stale replay is still dropped |
