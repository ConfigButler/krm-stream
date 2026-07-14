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

So a digest is not the mechanism for *"I don't want this"*. The mechanism for that is **a view plus
suppression** (§2, §3), and it needs almost no new protocol at all.

And for the *other* case — *"I may not see it, but I need to know it changed"*, which is the rotated
Secret — **a digest turns out not to be the mechanism either.** The gateway is stateful per stream
anyway (suppression requires it), so it can do the comparison itself and simply **say** the value
changed. Nothing derived from the secret ever leaves the process, and the entire cryptographic design
— HMACs, per-stream keys, offline-cracking risk — **evaporates**. §4.

Both cases then collapse into one vocabulary: a projection decides, per path, whether to **send**,
**withhold** (you learn it exists and when it changes, never what it is) or **drop** (it is as if it
did not exist). **"Redaction" is just `withhold` with a bad name.** §2.1.

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
| a digest in `metadata.annotations` | ❌ **no** — and this is the sharp one | an annotation **is part of the object**. A save carries it back and the gateway's private bookkeeping is now **persisted on the resource**, in etcd, forever. It is the mask landmine wearing a different hat. (§4 removes the digest entirely, so this temptation now has nothing to put there either) |

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

### 2.1 The unification: **there is no "redaction feature"** — there are three verbs

*"Can we drop the whole redaction thing by making the projection smarter?"* — and the answer is yes,
in the sense that matters: **redaction stops being a feature and becomes one of three things a
projection can say about a path.**

> ## A projection decides, per path, exactly one of:
>
> | verb | the value | you are told it exists | you are woken when it changes |
> |---|---|---|---|
> | **`send`** | you get it | — | ✅ |
> | **`withhold`** | **never leaves the gateway** | ✅ (`withheld[].path`) | ✅ (`withheld[].rev`) |
> | **`drop`** | never leaves the gateway | ❌ — it is as if it did not exist | ❌ **never. Zero events.** |

Everything in this document collapses into that table:

| path | verb | why |
|---|---|---|
| `metadata.managedFields` | `drop` | nobody wants it, and nobody wants to be woken by it |
| `Secret` `/data/*` values | **`withhold`** | *"I may not see it, but I want to know it rotated"* — the owner's case, exactly |
| `status`, for a status-blind editor | `drop` | the motivating case: **zero traffic** |
| `status`, for a dashboard | `send` | the product's headline |
| `spec`, for a pure status dashboard | `drop` | it never renders it |

**"Redaction" was just `withhold` with a bad name and its own plumbing.** `redactedPaths` becomes
`withheld[]`, and it carries the `rev` from §4 for free. One concept, one envelope field, one code
path — instead of a Secret-shaped special case bolted to the side of the projection.

### And it makes the central trade *explicit* rather than hidden

`withhold` and `drop` differ in exactly one way, and it is the finding from §0:

- **`drop` = zero events.** A change you are not told about cannot wake you.
- **`withhold` = one small event per change.** You asked to know, so you get told — and being told
  *costs an event*, which is the whole thing §0 warns about.

So a consumer choosing `withhold` for `status` is choosing "wake me on every status change but do not
send me the bytes". That may be exactly right for a list view with a *changed* dot — **and now it is a
choice someone makes on purpose**, in the projection's definition, rather than a consequence they
discover in production. The old design hid that trade inside the word "redaction". This one puts it in
the vocabulary.

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

## 4. "I want to know the Secret changed, not what it is" — and it needs no hash at all

This is the case the owner actually has, and the honest answer is much better than the one I first
wrote. **Do not publish a digest. Do not publish an HMAC. Publish nothing derived from the value.**

### The realization: the gateway already knows

We are about to make the gateway stateful per stream anyway — §3's suppression requires it to hold
the last thing it told each consumer. **So the gateway can do the comparison itself.** It does not
need to hand the browser a token so that the *browser* can compare. It can simply say:

> *"`/data/token` changed."*

That single sentence deletes the entire cryptographic section of the earlier draft, and every risk in
it.

### Why this is strictly better than a change token, not merely simpler

I proposed an HMAC keyed per-stream, so a token would be comparable *only within one stream*. Compare
the two, on the same footing:

| | a per-stream HMAC token | the gateway just says "changed" |
|---|---|---|
| *"the token was rotated"* | ✅ | ✅ |
| *"it changed while I was disconnected"* | ❌ — a new stream, a new key, no comparison possible | ❌ — same |
| *"these two Secrets have the same password"* | **✅ — and that is a DISCLOSURE we never wanted to grant** | ❌ (correctly) |
| offline-crackable oracle if the key ever leaks or is reused | a real risk to reason about, forever | **does not exist** |
| security review needed | yes | **none. Nothing derived from the value leaves the process** |

The token is **the same power for the legitimate use, plus one illegitimate one, plus a permanent
footgun**. Notice that even the *ideal* token — perfectly keyed, never leaked — buys nothing over
"changed", because both are useless across a reconnect. There is no version of the crypto that wins.

*(For the record, the risk that is now simply gone: a raw `sha256` of a Secret is an offline-crackable
oracle. Secrets are frequently low-entropy or structured — a password, a PIN, a token with a known
prefix. We would have disclosed the secret we were protecting while believing we had hidden it.)*

### The shape

Per path, per stream, a small integer that increments **when the withheld value actually changes**:

```jsonc
{
  "seq": 4712,
  "type": "modified",
  "object": { "kind": "Secret", "data": {} },
  "withheld": [
    { "path": "/data/token",    "rev": 3 },   // ← rotated twice since this stream began
    { "path": "/data/username", "rev": 1 }
  ]
}
```

A counter rather than a boolean, because a boolean is fragile: a UI that coalesces renders can miss
the one event that carried `changed: true`. A counter is *state*, so a consumer that re-renders late
still sees that `rev` moved. It leaks only what a boolean leaks over time — that something changed,
and when.

**The honest limit, stated in the spec:** `rev` is scoped to one stream. **You cannot know whether a
withheld value changed while you were disconnected** — and *no* design can tell you that without
publishing a stable, content-derived identifier, which is exactly the thing we must never publish. So
a consumer that cares treats **every `reset` as "this may have changed"**. That is a real limitation
and it is the correct one.

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
5. **`withheld[]` with `rev`** (§2.1, §4) — replacing `redactedPaths`, and giving the owner's case
   ("did the Secret rotate?") an answer that needs no crypto, no key management and no security
   review. It is a rename plus a counter.

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
