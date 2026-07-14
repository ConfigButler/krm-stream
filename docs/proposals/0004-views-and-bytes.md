# Proposal 0004 — views, suppression, and what a consumer never asked for

**Status:** design, for discussion. Nothing is built.

**The ask.** A controller rewrites `status` continuously. A consumer that only edits `spec` does not
care. Today it receives every one of those events, in full. That is bytes, wakeups and re-renders
spent on something nobody asked for — and it is the dominant traffic in this system, because status
churn *is* what Kubernetes does (F5).

**The answer, in one line:** the win is not sending fewer bytes per event. **It is sending no event
at all.** Everything else in this document is downstream of that sentence.

---

## 0. The finding that reshapes the proposal

The suggestion was: hash the interesting parts of the object, put the hashes in metadata, and let a
consumer decide what it wants. Two of those three are right, and the third is worth stating plainly
because it is counter-intuitive:

> ### ⚠️ A `status` digest and event-suppression are **mutually exclusive**.
>
> If the gateway emits a digest of `status`, then **every status change changes the digest** — so
> every status change still produces an event. You have saved the *bytes* of the status block, and
> kept **every wakeup, every SSE frame, every re-render and every reconnect-relevant event**.
>
> For the case that motivated this ("I don't care about status at all"), the correct traffic is
> **zero events**, not "a small event per change". A digest actively prevents the thing we want.

So digests are not the mechanism for *"I don't want this"*. They are the mechanism for *"I cannot be
shown this, but I need to know it changed"* — which is a real, narrower case (a rotated Secret), and
§4 designs for exactly that.

The mechanism for "I don't want this" is **a view plus suppression**, and it needs almost no new
protocol at all.

---

## 1. The invariant that must survive all of this

Proposal 0003 removed the `**REDACTED**` mask because it was a value *we invented* that a browser
could save back over a real Secret. The general rule behind that lesson is the one to write down now,
because both of this proposal's temptations violate it:

> ## The object is a **strict subset** of what the API server sent.
>
> The gateway may **remove** a key. It may never **add** one, and it may never **change** a value.
> Everything the gateway wants to say *about* what it removed lives in the **envelope**, next to the
> object — never inside it.

This single rule decides three open questions at once:

| tempting | verdict | why |
|---|---|---|
| a mask (`"**REDACTED**"`) in the value | ❌ already deleted (0003) | a value we invented, and a browser can save it back |
| a `maskedData:` field on the object | ❌ **no** | it is a *synthesized field*. Spec §2 forbids them, and for a concrete reason: a consumer round-trips the object into a patch, and now `maskedData` is written **to your cluster** |
| a digest in `metadata.annotations` | ❌ **no** — and this is the sharp one | an annotation **is part of the object**. A save carries it back and the gateway's private bookkeeping is now **persisted on the resource**, in etcd, forever. It is the mask landmine wearing a different hat |

The instinct behind `maskedData` — *be explicit, do not pretend a field is the field* — is exactly
right. The place to be explicit is the **envelope**: it is ours, it is not a Kubernetes object, and
nothing can round-trip it into a cluster.

---

## 2. Views: "I don't want status" is just a projection

We already have the machinery. `projection` is on every `reset`, it is server-declared, and it names
what the gateway removed. **A view is a projection.** No new concept:

| projection | object contains | for |
|---|---|---|
| `krm-raw/v1` | everything, minus machinery | debugging, operators |
| `krm-editor/v1` *(default)* | the above, minus Secret values | the editing case |
| **`krm-editor-nostatus/v1`** | the above, **minus `status`** | an editor that never renders status |
| **`krm-status/v1`** | `metadata` + `status` only, **no `spec`** | a status dashboard that never edits |

Two design rules, and both come straight from §8's existing stance:

- **Views are named and server-declared, never a caller-supplied field list.** An arbitrary
  `?omit=/spec/template/spec/containers/0` is unbounded input, an unbounded number of projection
  identifiers on the wire, and an unbounded cache/fan-out key space. The scope is *server-normalized*
  (§8); so is the view. Adding a named view is a code change, on purpose.
- **The consumer asks; the gateway decides.** `?view=nostatus` is a *request*. The `reset` echoes the
  projection actually in force, and a consumer that gets something else must believe the `reset`.

**This costs almost nothing to build**: `project()` already takes a `Projection` and returns
`redactedPaths`. A view is a `switch` arm.

---

## 3. Suppression: the actual win, and it is enormous

A view alone saves bytes. **A view plus suppression saves the entire event.**

```go
// after projecting, before emitting:
if digest(projected) == lastEmittedDigest[uid] {
    continue    // this consumer's world did not change. Say nothing.
}
```

Under `krm-editor-nostatus/v1`, a Deployment reconciling through a rollout produces **N status-only
MODIFIEDs upstream and zero events downstream**. Not smaller events. *No* events. No frame, no wakeup,
no re-render, no coalescing pressure, no bandwidth.

That is the whole ask, in about twenty lines, and it is *safe* — dropping an event whose projected
content is byte-identical to what the consumer already holds is a no-op by definition. The protocol's
convergence promise is untouched.

### The wrinkle, and it is a real one

`metadata.resourceVersion` **changes on every write**, including a status-only one. So two projections
that differ in nothing a consumer can see will still differ in `resourceVersion`, and a naive digest
suppresses nothing.

So the **suppression digest excludes `metadata.resourceVersion`** (as it already excludes
`managedFields`, which the projection removes anyway).

> **To be unambiguous, because this was read the other way: `resourceVersion` stays ON THE WIRE.** It
> is part of the object, and the object is what the API server sent. We do not delete it, and §1's
> subset rule does not ask us to. What is excluded is its participation in a *comparison we make
> internally*.

### 3.1 …but suppression makes the consumer's `resourceVersion` **stale**, and that has teeth

This is the part I under-sold, and the question that prompted this section is what exposed it.

If we suppress, the consumer keeps the object it already had — **including its old
`resourceVersion`**. Its *visible* content is correct and complete. But the `resourceVersion` it holds
now trails the cluster's, and it trails it **precisely in the case that motivated this proposal**: the
status-blind editor, watching an object whose `status` churns invisibly.

Now imagine a host that uses that `resourceVersion` for optimistic concurrency — a `PUT` or a patch
with a precondition. **Every save fails with a `409 Conflict`**, forever, on exactly the objects that
churn most. Worse, it is a *false* conflict: nothing the user could see has changed. That would be a
genuinely maddening bug, and it would look like ours.

The protocol already forbids the thing that causes it, and the fix is to say so louder rather than to
invent anything:

- **Spec §3 already forbids a whole-object `PUT`** built from a projected object. Saves are a
  constrained **merge patch**, and a merge patch carries no `resourceVersion`. A conforming consumer
  therefore never sends one.
- **Spec §6 already forbids a consumer parsing or ordering by `resourceVersion`.** It is opaque.
- What we must now add, explicitly: **a consumer MUST NOT use `resourceVersion` as a save
  precondition.** If a host wants optimistic concurrency, it does the read **server-side, at save
  time** — it has an API client and the browser does not. The browser's copy is *a view*, not a
  transaction handle, and it never was.

**So why keep it on the wire at all?** Because it is part of the object, it is priceless in devtools
and in a raw/operator view, and deleting a field to stop people misusing it is how you end up with a
protocol full of holes. Keep it, and be explicit — which is exactly what §8 is for.

### 3.2 A correction on what Kubernetes actually guarantees

Both looser and stricter than it is usually stated, and this repo has the receipts:

- **kube-apiserver, 1.35+: it IS guaranteed** to be orderable as a monotonically increasing integer —
  that is a *Certified Kubernetes conformance requirement*, for base API objects **and** CRDs, and it
  is the entire basis of this gateway's `OrderingStrict` default (see `stream.go`). So "never
  guaranteed increasing" is too pessimistic for the main case.
- **But the guarantee is per-storage, and it is not universal.** Our own cluster run (F6) found the
  aggregated API server (`wardle`) numbering **its own `resourceVersion` space from ~1**, in its own
  etcd — and because that etcd was an ephemeral sidecar, **a restart sends them backwards.** That is
  not a hypothetical: it is in `docs/facts/observed-v1.36.2+k3s1.md`.
- **And the API contract to clients says "opaque" regardless.** A guarantee the *server* must uphold
  is not a licence for a *client* to depend on it.

Which is the whole argument for §8: the gateway may reason about `resourceVersion` (carefully, with an
escape hatch, having verified it against a real cluster). **A browser never should — so give it a
number that is honestly ours.**

### It composes with what exists

`SharedBackend` fans one upstream watch out to N subscribers. Projection and suppression are
**per-subscriber**, downstream of the shared cache — so one watch can feed a status dashboard and a
status-blind editor at the same time, each seeing exactly what it asked for. Nothing about the sharing
changes.

### It composes with what exists

`SharedBackend` fans one upstream watch out to N subscribers. Projection and suppression are
**per-subscriber**, downstream of the shared cache — so one watch can feed a status dashboard and a
status-blind editor at the same time, each seeing exactly what it asked for. Nothing about the sharing
changes.

---

## 4. Digests — the narrow case where they *do* pay

Not for "I don't want it" (§0). For **"I may not see it, but I need to know it changed."** That is
the rotated-Secret case, and it is real: a UI wants to show *"the token was rotated 2 minutes ago"*
without ever holding the token.

**In the envelope. Never in the object** (§1). Replace the flat `redactedPaths` with something that
can carry it:

```jsonc
{
  "type": "modified",
  "object": { "kind": "Secret", "data": {} },
  "redacted": [
    { "path": "/data/token",    "changeToken": "k1:9f2c…" },
    { "path": "/data/username", "changeToken": "k1:04ab…" }
  ]
}
```

### It MUST be a salted MAC, not a hash — and this is not pedantry

A raw `sha256` of a Secret's value is **an offline-crackable oracle**. Secrets are frequently
low-entropy or highly structured — a password, a 6-digit PIN, a token with a known prefix. Publishing
`sha256(value)` to a browser hands an attacker a target they can grind at their leisure, and we would
have *disclosed the Secret we were protecting* while believing we had hidden it.

So:

- it is **HMAC(k, value)** with `k` random **per gateway process** (or per stream), never persisted;
- it is therefore **opaque and comparable only within one stream**. It is a *change token*, not a
  content identifier. Two streams do not agree on it; two gateways do not agree on it. That is the
  point, and the name should say so — `changeToken`, not `digest` or `hash`.
- **no length, no size.** The old gateway README offered a mask *"with length"*; the length of a
  password is exactly the sort of thing you do not publish.

**Cost, stated honestly:** emitting a change token for a Secret's data means a Secret whose data
changes now generates an event *for consumers that cannot see the data.* That is correct here (they
asked for it) — but it is the same trap as §0, and it must be opt-in per view, not on by default.

---

## 5. "Could we use a different protocol?"

Worth answering directly, because the honest answer is **almost certainly not**, and the reasoning
should be written down once so it is not relitigated.

The cheap wins are enormous and the expensive ones are small. In order:

| | win | cost | verdict |
|---|---|---|---|
| **1. Suppression** (§3) | status churn → **zero events** for a status-blind view | ~20 lines | **do it** |
| **2. Views** (§2) | fewer bytes, and it is what *enables* suppression | a `switch` arm | **do it** |
| **3. Coalescing** (gateway §8, already designed) | 200 events → 1 for a slow tab that *does* want status | already specified | do it |
| **4. HTTP compression** | KRM JSON is enormously repetitive; gzip/br is typically **5–10×**. **Invisible to the protocol** — no spec change, no client change | must flush per event or liveness dies | **do it, and measure before anything below** |
| **5. Deltas** (JSON Patch instead of whole objects) | large, for big objects under small changes | **breaks I-REPLACE** — the protocol's entire convergence proof rests on *replace, never merge*. Deltas need gap-free ordering, a shared base, and a recovery story for a lost frame. We would be trading a property we can prove for bytes we can get from gzip | **no** |
| **6. WebSocket / gRPC-web / CBOR** | binary framing, bidirectional | loses `EventSource`: native reconnect, same-origin cookie auth (docs/auth.md — the whole auth model hangs off SSE's constraints), trivial proxying. CBOR buys ~20–30% on unstructured JSON; **gzip buys more, for free** | **no** |

**The summary is uncomfortable and probably correct: the answer to "we are sending too many bytes" is
mostly "stop sending events nobody wants", and after that "turn on gzip".** A transport change is the
most expensive item on the list and the least effective, and it would cost us the browser story that
is the entire point of the project.

*(One caveat on gzip, for the record: compression + secrets is where BREACH/CRIME live. It matters
when an attacker can inject chosen content into the same compression context as a secret and observe
lengths. We no longer put Secret values on the wire at all (0003), which removes the interesting
target — but if a future view ever discloses a value, compression for that view needs a fresh look.)*

---

---

## 8. The envelope, and a sequence number that is honestly ours

**Yes. Do this, and do it now.** It is the cheapest thing in this document and it is the one with a
property nothing else provides.

We already *have* an envelope — `type`, `target`, `scope`, `projection`, `object`, `redactedPaths`,
`identity`, `code`, `terminal`. Nothing needs inventing. What it lacks is a number:

```jsonc
{ "seq": 4712, "type": "modified", "object": { … }, "redactedPaths": [] }
```

**`seq`** — a `uint64`, **per stream**, starting at 1, incremented by one for **every event actually
written** to that consumer. Strictly increasing. No gaps.

### What it buys, and the third one is the real reason

1. **It gives the consumer an order that is legitimately theirs to use.** Today a client that wants to
   reason about ordering has exactly one number available, and it is the one thing the spec forbids it
   from touching (§6, §3.2 above). That is a trap we set. `seq` removes the temptation by supplying the
   honest alternative — *and it is ours*, so no aggregated API server can restart and send it
   backwards.

2. **It makes suppression and coalescing legible.** `seq` is assigned **at emit time, not at
   generation time** — so a suppressed or coalesced event consumes no number, and the stream a
   consumer sees is gapless *by construction*. "I dropped 200 status events for you" is invisible, as
   it should be, rather than showing up as a suspicious hole.

3. **A gap is therefore proof of loss.** This is the one that is genuinely new. Today, if an
   intermediary truncates or drops an SSE frame, the consumer applies the events it *did* get,
   converges to a wrong state, and **never finds out**. It looks fine. With a gapless `seq`, a
   consumer that sees 4711 → 4713 *knows* it lost something, and can do the one correct thing:
   reconnect and take a fresh snapshot. We currently have no way to detect that at all.

### The rules that keep it honest

- **`seq` is per-connection, not per-cycle.** It does not reset on a `reset` — otherwise a gap across
  a resync would be invisible, which defeats (3).
- **It is NOT a resume token, and it MUST NOT go in the SSE `id:` field.** Spec §7 bans `id:` lines for
  a good reason: `EventSource` would replay it as `Last-Event-ID` on reconnect, promising a delta
  resume that v1 does not have. `seq` lives in the JSON envelope, where it makes no such promise. A
  reconnect starts a new stream, a new `seq`, and a fresh snapshot.
- **It is per-subscriber**, because suppression and coalescing are. Two tabs on the same scope have
  different `seq` streams, and that is correct — `seq` describes *this conversation*, not the cluster.
- **It is not a cluster clock.** It says nothing about other objects, other scopes, or other streams.
  Say so in the spec, or someone will build a distributed system on it.

### Cost

About twenty bytes per event and a counter. **And it is additive**, so it is compatible with the
unknown-fields rule (§0 of the spec) — but there is no reason to lean on that: we have no users, the
protocol is unreleased, and adding a field to the envelope now is free in a way it will never be
again. That is the actual argument for doing it *now*.

---

## 6. What I would build, in order

0. **`seq` in the envelope** (§8). First, because it is free *now* and never again, because it is the
   only thing here that can detect a lost frame at all, and because it is what lets us tell a consumer
   "never look at `resourceVersion`" while handing them something they *can* look at.
1. **Suppression** (§3), under the existing projections. Immediate, invisible, no protocol change —
   and it already helps: `krm-editor/v1` drops `managedFields`, so a `managedFields`-only update is
   already a no-op event we currently forward.
2. **The `resourceVersion` rules, stated** (§3.1). `resourceVersion` stays on the wire; a consumer MUST
   NOT use it as a save precondition; optimistic concurrency is done server-side at save time. Without
   this, suppression hands a host a `409` storm on exactly the objects it cares about — a false
   conflict, and it would look like our bug.
3. **`krm-editor-nostatus/v1` and `krm-status/v1`** (§2), plus the `view` scope parameter. With (1) in
   place, this is where "the frontend does not care about status" becomes *zero traffic*.
4. **Measure gzip.** Before anything in §5 below the line.
5. **`redacted[].changeToken`** (§4) — last, opt-in, and only if someone actually wants
   "the Secret rotated" in a UI. It is the smallest win and the only one with a security footgun.

## 7. Open questions

- **`data: {}` versus no `data` at all.** Today a fully-redacted Secret arrives with `data: {}`. Under
  the subset rule (§1) both are legal — removing map entries leaves an empty map; removing the key
  leaves nothing. `{}` says *"the data map exists and you may see none of its keys"*, which is true
  and slightly more informative. I lean towards keeping it, but the explicitness argument cuts the
  other way and it is worth one round of discussion.
- **Should a view change the *scope key*?** Two subscribers to the same scope with different views
  share an upstream watch (good) but not a projection. Nothing breaks; it is worth confirming that
  fan-out accounting does not accidentally key on the projection.
- **Does anyone actually want `krm-status/v1`?** It is the mirror of the motivating case and it is
  free once views exist. But an unused projection is a maintained projection.
