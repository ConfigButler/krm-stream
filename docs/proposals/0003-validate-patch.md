# Proposal 0003 — the redacted-value hazard: guard it, or stop creating it

**Status: ACCEPTED — option B, and it is built.** The projection no longer emits a placeholder; a
redacted value is deleted, and `redactedPaths` carries the fact. Spec §3.1 now **forbids** a mask on
the wire. The mask-overwrite hazard cannot arise, so `ValidatePatch` is not needed for it and is not
built.

> **What remains, and it is small:** a consumer could still patch a *projection-removed* path
> (`managedFields`, `last-applied-configuration`) — it never saw those either. That is worth refusing
> in a host's save endpoint (spec §3 says so), but it destroys nothing and it is not load-bearing. The
> catastrophic case is closed at the source.

**One sentence:** the projection invents `**REDACTED**`, an ordinary save can write it back *over the
real Secret*, and the only thing standing in the way is a paragraph — so either ship the guard
(`ValidatePatch`), or **stop inventing the poisoned value at all**, which is what I now think we
should do.

> **I changed my mind while writing this.** It opened recommending `ValidatePatch`. Then the owner
> asked whether the feature should exist at all — *"in the end, starting a watch on a Secret is a
> decision you make yourself"* — and answering that honestly showed the guard to be the wrong shape.
> `redactedPaths` already carries everything the mask carries. The mask is **redundant**, and it is
> the **sole** source of the hazard. See "Three options" below. The reasoning is left in rather than
> tidied away, because how the answer moved is part of the argument.

---

## What actually happens today

`krm-editor/v1` masks Secret values. That is the feature: keys-only disclosure. You may see *that*
`token` exists; you may not see it.

```jsonc
// what the browser holds, after the gateway's projection:
{ "kind": "Secret", "data": { "token": "**REDACTED**", "username": "**REDACTED**" } }
```

The browser never saw the real value. It saw `**REDACTED**` — a string this library invented
(`gateway.RedactedPlaceholder`).

Now the user edits a label and saves. If the patch that reaches the API server contains
`data.token: "**REDACTED**"`, then **the literal string `**REDACTED**` is written over the real
Secret.** The token is destroyed. The request looked, from the browser, entirely ordinary: a 200, a
save, a green tick.

**This failure cannot happen without us.** No mask, no hazard. The projection is what invents the
poisoned value, so the projection is what creates the obligation to check for it.

## Why "the client already handles it" is not an answer

It does handle it, and it is tested: `secret-redaction.yaml` asserts the mask is read-only, never
dirtiable, and never appears in `patch()`. The browser suite asserts it in a real Chromium.

That is a **necessary** condition and not a **sufficient** one, and the fixture says so in its own
words — it lists *three* obligations and calls the feature "a vulnerability" if any one fails:

> 3. the gateway REJECTS any patch that touches a redacted path — belt and braces, because **the
>    client is not the security boundary. A hostile client is just a client.**

A conforming client will not produce the bad patch. But the save endpoint is an HTTP endpoint. It
accepts whatever bytes are posted to it — by a hostile client, a stale vendored copy of the library,
a `curl`, a third-party consumer that read the spec and implemented it slightly wrong. Every one of
those is a client, and none of them are the boundary.

## What the repo says right now, and why that is thin

We removed `gatewayRejects:` from the corpus — correctly, because it asserted a rejection *nothing in
this repo performs*, and a conformance rule the gateway cannot execute is not a rule.

But removing the false claim also removed the pressure to close the gap. What replaced it is prose in
[spec/v1.md §3](../../spec/v1.md):

> **The endpoint that accepts a save MUST reject any patch touching a path in `redactedPaths` or a
> path the projection removed.**

That sentence is correct. It is also the weakest possible enforcement for a security check that
**every single adopter must implement identically and correctly**, and whose failure mode is
*silently destroying a Secret*. Prose does not fail CI. A function does.

## The proposal

### It is not a write path

This is the whole reason it is proposable at all, given that krm-stream is deliberately a read path:

```go
// gateway/validate.go — pure. No client. No connection. No API server. No I/O.
func ValidatePatch(projection Projection, obj KRMObject, patch map[string]any) *StreamError
```

It takes the object the consumer was shown, the patch they want to save, and answers one question:
**does this patch touch anything I hid from them?** It returns `FORBIDDEN` with the offending JSON
Pointer, or nil.

The host still performs the write, through the Kubernetes API, from its own handler, exactly as it
does today:

```go
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
    // ... your auth, your object, your patch ...
    if err := gateway.ValidatePatch(gateway.ProjectionEditor, obj, patch); err != nil {
        http.Error(w, err.Message, http.StatusForbidden)   // "/data/token is redacted"
        return
    }
    dyn.Resource(gvr).Namespace(ns).Patch(ctx, name, types.MergePatchType, body, opts)
}
```

Three lines in an adopter's handler, instead of a security check they must derive from a blockquote.

### It closes the two failure modes, not one

1. **A redacted path** (`/data/token`) — the mask-overwrite above.
2. **A projection-removed path** (`metadata.managedFields`, the last-applied annotation) — the
   consumer never saw these either, so a patch touching them is equally a write of something they
   were never shown. The spec already names both; only redaction gets discussed because its failure
   is the loud one.

### The fixtures were right; only their placement was wrong

`gatewayRejects:` becomes `hostRejects:`, back in `secret-redaction.yaml`, and it drives a real test
of a real function:

```yaml
hostRejects:
  - patch: { data: { token: "**REDACTED**" } }
    code: FORBIDDEN
    because: "/data/token is redacted under krm-editor/v1 — this writes the MASK over the real secret"
  - patch: { metadata: { managedFields: [] } }
    code: FORBIDDEN
    because: "the projection removed managedFields; the consumer never saw it"
  - patch: { metadata: { labels: { app: web } } }
    allowed: true
    because: "an unredacted field of the same object — keys-only disclosure must still let you edit"
```

That last case matters as much as the refusals: an over-eager guard that refuses *any* patch to a
Secret would break the feature it is protecting.

### And its TypeScript twin

The same table, read by the client suite, asserting `patch()` never *produces* a rejected patch. Two
ends, one corpus — exactly as `scopes.yaml` does for the request half. The client check stays what it
is (a good-citizen check); the Go one is the boundary.

## Cost

| | |
|---|---|
| `gateway/validate.go` | ~60 lines. Walk the patch; compare against `redactedPaths` + the projection's removal rules |
| fixtures | `hostRejects:` in one existing fixture, plus one non-Secret case |
| tests | Go conformance reads the fixture; TS asserts `patch()` never emits a rejected path |
| dependencies | **none.** The core module stays zero-dependency |

## The argument against, stated fairly

**"It is scope creep. We just decided krm-stream is a read path."** True, and this does not change
that: it holds no client and writes nothing. But it *is* one more thing in the API surface, and every
function a library exports is one it must keep. The counter-argument is that this particular function
is not a new capability — it is the enforcement of a rule the spec **already declares normative**, for
a hazard the library **already creates**.

**"The host can write it in ten lines."** It can. All of them can. And they must all get it right,
identically, forever — including the part where `redactedPaths` is authoritative and the *shape* of a
value is never evidence (a real value that merely looks like `**REDACTED**` is not redacted). That is
exactly the kind of thing a library exists to have gotten right once.

---

## "Should the feature exist at all? Watching a Secret is my decision anyway."

This was the owner's question, and it is the right one to ask before adding a guard. It deserves a
straight answer rather than a defence of the status quo.

**The premise is correct.** Streaming a Secret is decided **twice**, by other people, before this
library ever sees it:

1. the host **allowlists `secrets`** in `ScopePolicy` — which is deny-by-default, so this is a
   deliberate line of code somebody wrote;
2. **Kubernetes RBAC** grants that user `list`/`watch` on Secrets in that namespace — and if it does
   not, the API server refuses and nothing we do matters.

A user who passes both of those can already run `kubectl get secret -o yaml`. So **the mask grants no
security against the user**. We say this ourselves, in this repo, in `docs/auth.md`: *a projection is
not authorization.* Anyone reaching for redaction as an access-control mechanism has already made a
mistake.

So what *is* the mask for? Exactly one thing, and it is worth naming precisely, because the whole
decision turns on whether you buy it:

> **A browser is an ambient, leaky environment in a way a terminal is not.** `kubectl get secret` is a
> deliberate act by one person at one moment. A dashboard tab is *left open* — on a second monitor, in
> a screen share, in a screenshot pasted into a ticket, in a browser extension's DOM access, in an
> error reporter that serializes application state, in a projector at a standup. The same value, in
> the same eyes, at a different exposure profile.

That is why every serious Kubernetes UI (Lens, Headlamp, the OpenShift console) hides Secret values
behind an explicit *reveal*, even though every one of their users could `kubectl get` the same value
in five seconds. It is not access control. It is **not putting credentials on a screen nobody asked to
have them on**.

### But that argues for redaction, not for the *placeholder*

And here is the thing that changes my answer. Look at what the mask actually buys, versus what carries
the information:

- **`redactedPaths` is authoritative, mandatory on every `added`/`modified`, and already carries the
  fact.** The client marks paths read-only from *that* ([`store.ts`](../../packages/krm-stream/src/store.ts)),
  not from the value. A UI can render `token ••••••` from `redactedPaths` alone.
- Spec §3 **already** says a consumer MUST NOT treat an absent path as deleted *if that path appears
  in `redactedPaths`*. **Omitting the value is already legal, today, with no spec change.**
- The placeholder is therefore *redundant* with `redactedPaths` — and it is the **sole** source of the
  overwrite hazard. It is the only poisoned value in the system, and we invented it.

Compare the two under the sanctioned write (an RFC 7386 merge patch, which is what `patch()` builds):

| | consumer sends `data: {token: <mask>}` | what reaches the Secret |
|---|---|---|
| **placeholder** (today) | the literal string `**REDACTED**` | the real token is **destroyed** |
| **omitted** | there is no such value to send — the draft never had one | nothing. The token is untouched |

The hazard does not need a guard. **It needs to not be created.**

## Three options, and what I actually think

| | | hazard | keys-only disclosure | cost |
|---|---|---|---|---|
| **A** | keep the placeholder, add `ValidatePatch` | guarded | yes | +60 lines, +fixtures, +a rule every adopter must not forget |
| **B** | **keep redaction, drop the placeholder** — omit the value; `redactedPaths` carries the fact | **cannot arise** | yes (from `redactedPaths`) | a UI reads two fields instead of one |
| **C** | delete redaction entirely | cannot arise | no — plaintext tokens in the DOM | −1 feature, −1 spec section |

**I recommend B**, and I have changed my mind to get there: I opened this proposal recommending A,
and writing out the owner's question is what showed A to be the wrong shape. A is a guard for a
hazard we did not have to create. B removes the hazard *and* keeps the property that made the feature
worth having — and it needs no spec change, because §3 already anticipated it.

**On C, honestly:** it is coherent, it is simpler, and the premise behind it is right — this *is* the
host's decision, and the mask is not protecting them from anything they could not already read. If we
were arguing about a CLI I would agree with deleting it outright. What stops me is the exposure
asymmetry above: the default projection is `krm-editor/v1` precisely so that somebody who allowlists
`secrets` without thinking hard does not thereby stream plaintext credentials into every open tab,
screen share and screenshot in the building. Safe-by-default is worth one field of the wire. And the
blast radii are not symmetric: the cost of B being wrong is a UI that has to look at one extra array;
the cost of C being wrong is a token in a screenshot, and you never find out.

If B still feels like too much, C is the *next* most defensible position — far more defensible than A.
The one position I would argue against is the status quo: **inventing a poisoned placeholder and
leaving the guard to a blockquote.**

## Recommendation

1. **Do B.** Stop writing `**REDACTED**` into the object; keep `redactedPaths`. The hazard stops
   existing, which is strictly better than guarding it. (`RedactedPlaceholder` stays exported and
   deprecated, since a consumer may render it.)
2. **Then `ValidatePatch` is optional, and much smaller.** With no placeholder, the catastrophic case
   is gone. What remains is a consumer patching a *projection-removed* path (`managedFields`) — worth
   refusing, but not worth losing sleep over. Ship it if it is cheap; it is no longer load-bearing.

If we do A instead, ship the guard. If we do C, delete the projection *and* the `redactedPaths`
plumbing, and say plainly in the README that a Secret in scope is a Secret on the screen.
