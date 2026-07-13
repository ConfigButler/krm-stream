# Authentication & authorization

**krm-stream never holds a credential, and it is not an authorization boundary. Kubernetes is.**

That is the whole stance. Everything below is a consequence of it, plus the one physical constraint
that decides how a browser can authenticate at all.

---

## The constraint that decides everything

**A browser's `EventSource` cannot send an `Authorization` header.** It is not an oversight we can
work around; it is what the API is. So a browser holding an OIDC access token *cannot put it on a
native SSE request*.

Everything else follows from that one sentence.

## The supported route: OIDC via Dex, with a same-origin cookie

There is exactly one supported deployment in v1, and it is the one `EventSource` permits:

```mermaid
sequenceDiagram
    autonumber
    participant B as Browser
    participant S as Your Go server<br/>(krm-stream gateway)
    participant D as Dex (OIDC)
    participant K as Kubernetes API

    B->>S: 1. GET /  (no session)
    S->>D: 2. OIDC redirect
    D-->>B: 3. user logs in
    B->>S: 4. callback with code
    S->>D: 5. exchange code → id/access token
    Note over S: your server CUSTODIES the token.<br/>krm-stream never sees, stores or logs it.
    S-->>B: 6. Set-Cookie: session (HttpOnly, SameSite)

    B->>S: 7. EventSource("/resource-stream/v1?…")<br/>carries the cookie, and nothing else
    S->>S: 8. Principal(r) — cookie → session → this user + their token
    S->>K: 9. ClientFor(target, principal) — a client bearing THEIR token
    K-->>S: 10. watch … or 403, if their RBAC says no
    S-->>B: 11. reset · added · synced · …
```

The browser authenticates to **your** server. Your server custodies the token. The SSE request
carries **nothing but a same-origin `HttpOnly` cookie** — no token in JavaScript, no token in a URL,
nothing an XSS can read.

Then step 9 is the one that matters: **the upstream watch is opened as the user.** If they may not
watch Secrets in that namespace, the API server refuses. No bug in this library can change that.

> A fetch-based reader (`connectResourceStream`) *can* send an `Authorization` header, for a browser
> that holds its own token. It exists, but it is **not the supported deployment in v1** — the cookie
> route above is, and it is the one the corpus and the browser suite actually exercise.

## The three seams, and what each is for

```go
gateway.Handler(gateway.Options{
    // WHO is calling? Your cookie → your session → your user. Opaque to us.
    Principal: func(r *http.Request) (gateway.Principal, error) { return sessionUser(r) },

    // MAY they? Checked BEFORE any watch opens, and again on every snapshot cycle.
    Authorizer: myAuthz,

    // Reach the cluster AS them — their token, their RBAC, their audit trail.
    Clients: func(target string, p gateway.Principal) (gateway.Backend, error) {
        return kube.NewBackend(dynamicClientBearing(p.(*User).Token)), nil
    },
    Scopes: myScopePolicy,
})
```

| seam | what it is | what it is **not** |
|---|---|---|
| `Principal` | whatever your session says the caller is. The library treats it as opaque (`any`), and never inspects, persists or logs it | not a credential *we* manage |
| `Authorizer` | **fail-fast, defence in depth.** Denies *before* the watch opens, so the existence of an object is never leaked to someone who may not see it | **not the boundary** — see below |
| `ClientFor` | the boundary. It hands back a client acting **as the caller**, so Kubernetes' own RBAC enforces | not a place to put a privileged god-client (unless you have read the sharing section) |

The gateway holds **no privileged client of its own**. It therefore cannot bypass RBAC even if it had
a bug that wanted to — authorization is not something this library *does*, it is something it
structurally *cannot avoid delegating*.

### Bearer token or impersonation?

`ClientFor` supports both, and it is a real choice:

- **The user's bearer token** — the blast radius is exactly that user's. Preferred.
- **Impersonation** (a service account sending `Impersonate-User`) — keeps Kubernetes as the boundary
  just as well, but requires your server to hold impersonate rights, which is a large privilege whose
  compromise is total.

## Long streams, short tokens

An SSE stream lives as long as an open dashboard tab — **hours**. An OIDC access token lives 5–60
minutes. So the credential you captured when the stream opened is *not* one you may keep using.

The gateway therefore **re-authorizes on every snapshot cycle**, and re-invokes `ClientFor` there
too:

- **Revocation is noticed.** Take a user's access away and their open stream ends with a **terminal
  `FORBIDDEN`**. Terminal matters: `EventSource` reconnects on its own, so a non-terminal refusal
  would leave a revoked user hammering a forbidden scope forever.
- **`ClientFor` is your refresh point.** It is called again each cycle, so you can hand back a client
  bearing a *fresh* token.

**Be honest about the gap:** a perfectly quiet stream may not cycle for a long time, so revocation is
noticed at the *next cycle*, not instantly. The credential half of that is solved properly on your
side of the seam — give the client a **refreshing token source** (Dex issues a refresh token; the
standard `oauth2.TokenSource` wraps it), and it never hands us a dead token in the first place.

## Two things that are easy to confuse

**A projection is not authorization.** Redaction is a *tighter disclosure layer on top of* RBAC: a
user who is fully entitled to read a Secret still does not get its value in a browser. It must
**never** be relied on to hide something the caller could not have read anyway — that is Kubernetes'
job. Confusing the two is how you end up with a "secure" viewer whose only protection is a mask.

**Sharing a watch moves the boundary.** `SharedBackend` opens one upstream watch per scope, so it
opens it **once**, so it opens it as **one identity** — your service account. At that moment your
`Authorizer` stops being defence in depth and becomes *the only thing* between a caller and the
objects. That is why it is opt-in, and why it is not the default. If you turn it on, your `Authorizer`
must be as strong as Kubernetes' RBAC was — the natural way to do that is to *ask Kubernetes*, with a
`SubjectAccessReview`, before serving a subscriber from the shared cache. (Not shipped yet; see
"Known gaps".)

## What this library never does

- It never **mints, refreshes, stores, inspects or logs** a credential.
- It never accepts an API-server address, endpoint or credential **from the caller** — such a query
  parameter is *refused*, not ignored (spec §8.1).
- It never **writes**. Saves go through the Kubernetes API from your own handler ([spec §3](../spec/v1.md)) —
  and *your* save endpoint carries the duty to refuse a patch touching a redacted path, because a
  mask written back would overwrite the real Secret.

## Known gaps

- **`SubjectAccessReview` authorizer** (`kube.SSARAuthorizer`) — the thing that would let you share a
  watch *and* keep Kubernetes as the authorization boundary. Not built.
- **`ValidatePatch`** — the redaction guard for your save endpoint, as a pure function rather than a
  paragraph. Today every adopter must implement it identically and correctly from prose, which is the
  weakest possible enforcement for a check that destroys a Secret when it is missed.
