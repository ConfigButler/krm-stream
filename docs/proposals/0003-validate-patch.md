# Proposal 0003 — `ValidatePatch`: the guard for a hazard this library creates

**Status:** proposed. **Not built.** This document exists because the decision is the owner's, and it
should be made against the actual hazard rather than against a summary of it.

**One sentence:** ship the redaction check as a **pure function** the host calls inside its own save
handler — no client, no connection, no API server, no write path — because the check is currently
enforced by a paragraph, and a paragraph does not fail CI.

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

## Recommendation

**Ship it.** The library invents the mask; the library should hand you the guard. The read-path stance
is preserved in full — no client, no connection, no write.

If the answer is no, then the honest alternative is to **stop masking**: a projection that removes
Secret values entirely (rather than replacing them with a poisoned placeholder) creates no
mask-overwrite hazard at all. That is a real option, and a coherent one. What is not coherent is
inventing the placeholder and leaving the guard to prose.
