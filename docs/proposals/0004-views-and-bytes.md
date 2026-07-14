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
**redact** (you learn the path exists and when it changes, never what it is) or **ignore** (it is as
if it never existed, and it never wakes you). **"Redaction" was never a feature — it is one of three
verbs.** §2.

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

## 2. There is no "redaction feature". There are three verbs.

*"Can we drop the whole redaction thing by making the projection smarter?"* — yes, in the sense that
matters. **Redaction stops being a feature and becomes one of three things a projection says about a
path.**

> ## A projection decides, per path, exactly one of:
>
> | verb | the value | you are told the path exists | you are woken when it changes |
> |---|---|---|---|
> | **`send`** | you get it | — | ✅ |
> | **`redact`** | **never leaves the gateway** | ✅ (`redacted[].path`) | ✅ (`redacted[].rev`) |
> | **`ignore`** | never leaves the gateway | ❌ — it is as if it never existed | ❌ **never. Zero events.** |

### On the naming, because two earlier drafts got it wrong

The first draft called these `withhold` and `drop`. The second, `redact` and `remove`. The candidates
were then `send` / `inform` / `ignore`. Working through it is worth recording, because the two verbs
are decided by *different* arguments:

**`redact`, not `inform` or `withhold`.** A redacted document has a *black bar*: you can see something
was taken, and you cannot see what. That is exactly this verb, and exactly what `redactedPaths` has
always meant — so `withhold` was inventing a word for a concept that already had one.

`inform` is the more interesting rival, and it loses on two counts. It is **ambiguous** — read
`Inform: ["/data/token"]` cold and ask *inform whom, of what?*; `Redact: ["/data/token"]` is
unmistakable. And, more importantly, **`redact` is a security term of art, and that is the point**:
the word is what catches a reviewer's eye scanning a diff, and it carries *"this is secret"* without
anybody reading the documentation. `inform` sounds like a notification preference. It is the same
argument that keeps the disclosing projection called `raw` rather than `verbose` (§2.1) — **the scary
word does its work precisely on the person who is not reading carefully.**

*(`inform` does name one real thing that `redact` does not: our redaction now tells you **when the
value changed**, which plain redaction does not imply. But that gap closes for free in the envelope —
`redacted: [{path, rev}]`, with the counter right there, self-documenting. The security legibility
`inform` would cost cannot be bought back that way, because its whole value is working on someone who
is not reading.)*

**`ignore`, not `remove`.** Here the owner's word is simply better, and it took a moment to see why.
`remove` describes what happens to the **object** — the key is gone. But the thing this proposal is
*about* is what happens to the **events**: a change there must **never wake you**. `remove` merely
*implies* that (by way of suppression); **`ignore` states it.** It is the stronger claim and the one a
reader actually needs.

So: **`send` / `redact` / `ignore`** — one verb chosen for what it does to the object's *value*, one
for what it does to the *stream*, and both for saying so out loud.

Everything in this document collapses into the table:

| path | verb | why |
|---|---|---|
| `metadata.managedFields` | `ignore` | nobody wants it, and nobody wants to be woken by it |
| `Secret` `/data/*` values | **`redact`** | *"I may not see it, but I want to know it rotated"* — the owner's case, exactly |
| `status`, for a status-blind editor | `ignore` | the motivating case: **zero traffic** |
| `status`, for an editor or a dashboard | `send` | the product's headline |

`redactedPaths` becomes `redacted[]` and carries the `rev` from §4 for free. **One concept, one
envelope field, one code path** — instead of a Secret-shaped special case bolted to the side.

### It makes the central trade *explicit* rather than hidden

`redact` and `ignore` differ in exactly one way, and it is the finding from §0:

- **`ignore` = zero events.** A change you were never told about cannot wake you.
- **`redact` = one small event per change.** You asked to know, so you get told — and *being told costs
  an event*, which is the whole thing §0 warns about.

So `redact` on `status` would mean "wake me on every status change, but do not send me the bytes".
Someone might want that for a list view with a *changed* dot — **and now it is a choice made on
purpose**, in a projection's definition, rather than a consequence discovered in production. The old
design hid that trade inside the word "redaction". This one puts it in the vocabulary.

---

## 2.1 The projections, and there are three

Short list, on purpose. Each one is a **code change** to add, which is the point (§2.2).

| projection | `metadata` | `spec` | `status` | `Secret` values | for |
|---|---|---|---|---|---|
| `krm-raw/v1` | send | send | send | **send** ⚠️ | an operator console that is *meant* to disclose |
| **`krm-full/v1`** *(default)* | send | send | send | **redact** | the editing case, and the live-status case. Both. |
| **`krm-spec/v1`** | send | send | **ignore** | redact | an editor that never renders status → **zero traffic** under status churn |

*(All three `ignore` `managedFields` and the last-applied annotation. Nothing wants those.)*

### Two naming notes, one of which is a safety property

- **The disclosing projection must have a frightening name.** `krm-raw/v1` is the one that puts Secret
  values in a browser. It should *sound* like the loaded gun it is, because the name is what shows up
  in a code review. **"verbose" fails that test** — it sounds like a log level. Keep `raw`, or call it
  `krm-unredacted/v1`, but do not make it sound benign.
- `krm-full/v1` and `krm-spec/v1` say what you *get*, which is what a caller is choosing between.

### Why there is **no status-only projection**

An earlier draft proposed `krm-status/v1` — `metadata` + `status`, no `spec` — for a dashboard that
never edits. **Dropped, and the owner's instinct was right for a reason worth writing down:**

**An object with its `spec` removed is not a KRM object any more.** It is a fragment that looks like a
resource and is not one — you cannot round-trip it, you cannot diff it against the cluster, and every
consumer that receives one has to know it is holding half a thing. This project's entire thesis is
*"the payload is a Kubernetes object — not an abstracted document"* (spec §0). A projection that
ignores `spec` quietly abandons that.

**And the byte argument for it is weaker than it looks.** The case *for* it is: under `I-REPLACE`,
every status event re-sends the whole object, *including a `spec` that did not change* — and a
Deployment's `spec.template` is large. But an SSE stream is one compression stream with one sliding
window, so a re-sent identical `spec` is mostly a back-reference and costs **far less than its size**.
**With a caveat worth stating rather than glossing:** gzip's window is **32 KB**. On a single-object
stream the previous `spec` is certainly still in it; on a **large scope with many objects interleaved**
it may have been evicted, and the saving degrades. (Brotli's window is far larger, which would restore
it.) So compression *blunts* this cost rather than eliminating it — **and the honest answer is that
nobody here has measured it.** This repo has a habit for exactly that (F1–F7): measure it before
building a projection to fix it.

The argument that actually kills `krm-status/v1` is the one above it, and it does not depend on
compression at all: **the payload would not be a KRM object.**

So: pay for `ignore` when you want **zero events** (that is `krm-spec/v1`, and it is a real win that
compression cannot give you). Do not pay for it merely to shave repeated bytes — gzip already did.

## 2.2 Custom projections: yes, but only the **host** may define one

Worth doing, and cheap — but the shape matters, because **a projection is a security policy**. It is
the thing that redacts Secrets.

- **A caller-supplied projection is never acceptable.** Not `?omit=…`, not a JSON-Pointer list, not
  ever. If the browser can define the projection, the browser can define one that does not redact —
  and it has just un-redacted your Secrets. It is also unbounded input, an unbounded set of projection
  identifiers on the wire, and an unbounded cache-key space. The scope is server-normalized (spec §8); so is
  the projection.
- **A host-registered projection is fine**, and is the flexibility that is actually wanted.

**But the rule shape must carry a KIND, and an earlier draft's did not.** This is worth catching now,
while it is a struct in a document rather than a released API:

```go
// ❌ the draft. Kind-blind, and therefore WRONG.
Rules{ Redact: []string{"/data"} }
```

`/data` is a path, and **`ConfigMap` has one too.** Select that projection for a ConfigMap scope and
you have just redacted every ConfigMap's contents — a rule meant for Secrets, silently applied to the
one kind whose whole purpose is to be readable. The built-in it is replacing is *not* path-scoped: it
reads `if p == ProjectionEditor && isSecret(out)`. **A rule that cannot express `isSecret` cannot
replace the thing it is replacing.**

```go
// ✅ rules are matched on the KIND first, then the path.
gateway.Handler(gateway.Options{
    Projections: map[gateway.Projection][]gateway.Rule{
        "acme-console/v1": {
            {Group: "", Kind: "Secret", Redact: []string{"/data", "/stringData"}},
            {Ignore: []string{"/status"}},   // no Kind ⇒ every kind
        },
    },
    // …and the caller may only ASK for a name that is registered.
})
```

`Group` matters as much as `Kind`, and leaving it out is the same bug one level down: a CRD may
perfectly well define its own `Secret` kind in its own group, and a rule that matches on kind alone
would redact it — or worse, *fail* to redact the real one because someone wrote `Kind: "secret"`.
Match on `(group, kind)`, exactly as Kubernetes does.

The rule, in one line: **the consumer picks a projection from a list; it never describes one.** The
`reset` echoes the projection actually in force, and a consumer that gets something other than what it
asked for must believe the `reset`.

*(Agreed this is not the first thing to build — but the seam should exist from the start, because
retrofitting "the host may define one" onto a hard-coded `switch` is a refactor, and reserving it now
is free.)*

---

## 3. Suppression: the actual win, and it is enormous

A view alone saves bytes. **A view plus suppression saves the entire event.**

```go
// after projecting, before emitting:
if digest(eventWeWouldSend) == lastEmittedDigest[uid] {
    continue    // this consumer's world did not change. Say nothing.
}
```

> ### ⚠️ Suppress on the EVENT, never on the OBJECT. This is not a detail — it is a bug I shipped in
> the first draft of this document, and it deleted §4 on exactly the motivating case.
>
> The first draft said `digest(projected)` — the projected **object**. Now rotate a Secret's token
> under `krm-full/v1`. The projection *deletes* the value, so the object is **byte-identical before
> and after**, and so is `redactedPaths`. **Verified, not assumed:**
>
> ```text
> before: {"apiVersion":"v1","kind":"Secret","metadata":{…},"type":"Opaque"}  redactedPaths=[/data/token]
> after:  {"apiVersion":"v1","kind":"Secret","metadata":{…},"type":"Opaque"}  redactedPaths=[/data/token]
> ```
>
> So the event is suppressed, the `rev` bump never arrives, and *"did my Secret rotate?"* returns
> **zero events, forever.** It would have looked exactly like the feature working.
>
> **The rule, stated so the next envelope field cannot re-introduce it:** the digest covers
> **everything the consumer would observe** — the object *and* the envelope (`redacted[]` and its
> `rev` vector, and whatever we add next) — minus only the fields that **churn without informing**:
> `metadata.resourceVersion` (§3.1) and `seq` itself (§6, which is assigned *after* this decision).
>
> Stating it over the object was the mistake. State it over the event, and the class of bug is closed
> rather than the instance.

Under `krm-spec/v1`, a Deployment reconciling through a rollout produces **N status-only
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

- **Spec §3 already forbids a whole-object `PUT`** built from a projected object. Saves are a
  constrained **merge patch**, and a merge patch carries no `resourceVersion`. A conforming consumer
  therefore never sends one.
- **Spec §6 already forbids a consumer parsing or ordering by `resourceVersion`.** It is opaque.
- And now, explicitly: **a consumer MUST NOT use `resourceVersion` as a save precondition.**

### 3.1.1 …and "just read it server-side at save time" is not an answer. It is a hand-wave.

An earlier draft ended the section there, saying a host wanting optimistic concurrency should *"read
server-side, at save time."* **That is wrong, and it is worth saying why, because it sounds
responsible.** A server-side read at save time returns the *current* `resourceVersion` **by
definition** — so a precondition built from it always matches. It cannot detect that the **user's
view** was stale, which is the only thing optimistic concurrency is for. It is not OCC. It is a write
with a ceremony.

The honest answer has three parts, and only the last one is a precondition:

**1. We already do better than OCC, and it is the product.** The store three-way merges and raises a
**live conflict** the moment the server changes a field the user is editing — *before* they save, while
they can see it. A save-time precondition is a strictly worse version of a thing this library already
does continuously. If you are reaching for OCC to solve "someone else changed it", you have skipped
the feature.

**2. A merge patch is per-field last-write-wins, and that is intended.** It carries only the fields the
user actually edited, so two people editing *different* fields of the same object both succeed — which
is correct, and is the whole reason the write is a patch and not a `PUT`. Say this plainly rather than
leaving people to infer it.

**3. If you want a hard precondition, `metadata.generation` — never `resourceVersion`.** `generation`
moves **only on `spec` writes**, so status churn never stales it and suppression cannot either. The
browser already holds it. A host compares the `generation` the browser had against the live one at save
time and rejects if it moved: **a true conflict, never a false one.** That is the rule §3.1 was missing.

> **But do not oversell `generation`, because it has two holes** (the reviewer named one; the second
> matters more for *this* product):
>
> - **It does not exist on `ConfigMap` or `Secret`.** No spec/status split, no `generation`. For those,
>   (2) is the answer and it is a fine one.
> - **It does not move on `metadata` writes.** Labels and annotations are `metadata`, not `spec` — and
>   editing a label is the *first thing* this library's own fixtures demonstrate. So a `generation`
>   precondition silently misses a concurrent label edit entirely. It is a precondition on `spec`, and
>   it should be described as exactly that and nothing more.

**So why keep it on the wire at all?** Because it is part of the object, it is priceless in devtools
and in a raw/operator view, and deleting a field to stop people misusing it is how you end up with a
protocol full of holes. Keep it, and be explicit — which is exactly what §6 is for.

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

Which is the whole argument for §6: the gateway may reason about `resourceVersion` (carefully, with an
escape hatch, having verified it against a real cluster). **A browser never should — so give it a
number that is honestly ours.**

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

Per path, per stream, a small integer that increments **when the redacted value actually changes**:

```jsonc
{
  "seq": 4712,
  "type": "modified",
  "object": { "kind": "Secret", "data": {} },
  "redacted": [
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
redacted value changed while you were disconnected** — and *no* design can tell you that without
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
| **3. Coalescing** (gateway README §8, already designed) | 200 events → 1 for a slow tab that *does* want status | already specified | do it |
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

## 6. The envelope, and a sequence number that is honestly ours

**Yes. Do this, and do it now.** It is the cheapest thing in this document and it is the one with a
property nothing else provides.

We already *have* an envelope — `type`, `target`, `scope`, `projection`, `object`, `redactedPaths`,
`identity`, `code`, `terminal`. Nothing needs inventing. What it lacks is a number:

```jsonc
{ "seq": 4712, "type": "modified", "object": { … }, "redacted": [] }
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

## 7. What I would build, in order

> **The order was wrong, and the reason is the bug in §3.** The draft had suppression at step 1 and
> `redacted[].rev` at step 5 — four steps apart. But suppression *silently deletes* `rev` unless the
> digest already covers it, and the deletion **looks exactly like the feature working**: zero events on
> a Secret nobody is rotating, and zero events on a Secret somebody is. Nobody would notice for months.
>
> **They are one change, and they ship together.** Whatever the sequence, the digest must cover the
> whole event *before* anything is allowed to depend on it.

1. **`seq` in the envelope** (§6). Still first: free *now* and never again, and the only thing here
   that can detect a lost frame at all.
2. **Suppression, digesting the whole EVENT** (§3) — and `redacted[].rev` **in the same change** (§4).
   Not adjacent. *Together.* The digest covers the object plus the envelope minus
   (`resourceVersion`, `seq`), and `rev` is part of the envelope from the first commit, so it can never
   be the thing that suppression quietly ate.
3. **The `resourceVersion` rules, stated** (§3.1): it stays on the wire; a consumer MUST NOT use it as
   a save precondition; a hard precondition is `metadata.generation` (spec-only, absent on
   ConfigMap/Secret), and the live conflict the store already raises is better than either. Without
   this, suppression hands a host a `409` storm on exactly the objects it cares about.
4. **`krm-spec/v1`** (§2.1), plus the `projection` scope parameter. With (2) in place, this is where
   "the frontend does not care about status" becomes *zero traffic*.
5. **Measure gzip** — and measure it on a *large scope*, not one object, because that is where the
   32 KB window stops saving you (§2.1). Before anything in §5 below the line.
6. **Host-registered projections** (§2.2), with rules matched on `(group, kind)` and then path.

## 8. Open questions

- ~~**`data: {}` versus no `data` at all.**~~ **RESOLVED: drop it, and it is now shipped.** I leaned
  towards keeping the empty map. The owner's reductio settles it in one line: *under the same logic
  you would send `status: {}` when status is ignored — which is bullocks.* Correct, and the rule was
  **already in our own code**, four lines above the offending block: `project()` removes an
  `annotations` map it has emptied, with the comment *"a map that is empty ONLY because we emptied it
  is our artifact, not the server's state."* We wrote the rule and then broke it for `data`. A
  fully-redacted Secret now carries **no `data` key at all**; `redactedPaths` says why, and the
  browser suite proves the mask still renders from it. (A container the *server* sent empty is kept —
  we remove what we removed, and nothing else.)
- **Should a view change the *scope key*?** Two subscribers to the same scope with different views
  share an upstream watch (good) but not a projection. Nothing breaks; it is worth confirming that
  fan-out accounting does not accidentally key on the projection.
- **Memory, for a large scope.** A joiner to a 5,000-object scope now materialises the whole snapshot
  at once (see the `SharedBackend` snapshot/live split). That is inherent to *any* snapshot — it is
  what the consumer asked for — but the number should be measured rather than assumed.
