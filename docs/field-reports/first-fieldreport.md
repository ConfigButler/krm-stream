# krm-stream feedback from krm-foyer

krm-foyer is about to host the krm-stream gateway with user-authenticated watches
([roadmap step 5](https://github.com/ConfigButler/krm-foyer/blob/main/docs/roadmap.md#order-of-work)). This note asks for five changes, most
important first. None of them blocks us: for each we say what krm-foyer does meanwhile.
Everything was checked against krm-stream v0.4.0 (`gateway`, `gateway/kube` and
`@configbutler/krm-stream`) on 2026-10-02, by reading the source and, for the error
output below, by running the gateway.

## The asks at a glance

| # | Ask | Priority | What krm-foyer does meanwhile |
| --- | --- | --- | --- |
| 1 | Map Kubernetes errors to the spec's codes, and recover from the retryable ones with backoff | High | Wraps `kube.Backend` and maps errors itself |
| 2 | Send a generic message with an unexpected `INTERNAL`, and give the detail to a diagnostic hook | High | The same wrapper keeps raw errors from reaching the gateway |
| 3 | Let `Handler` serve a host that delegates resource admission to Kubernetes | Medium | Parses the scope itself and calls `Gateway.ServeStream` |
| 4 | Let `Principal` say why it failed, so a missing credential is `UNAUTHENTICATED` | Low | Answers before the gateway runs, so it never sees this |
| 5 | Honour retry hints at the layer that retries | Low | Nothing needed for now |

## How krm-foyer uses krm-stream

krm-foyer is a backend for frontend: it signs users in with OIDC, keeps their ID token
server-side behind a session cookie, and proxies `/k8s` to the API server with that
token. **Kubernetes alone decides what a user may do:** krm-foyer has no allowlist of
its own, and no service-account fallback. Its [design](https://github.com/ConfigButler/krm-foyer/blob/main/docs/design.md#access) promises
that for `/stream` as well as `/k8s`.

So krm-foyer uses the gateway in per-user mode:

- **Before the gateway,** krm-foyer checks the session and answers 401 itself when there
  is none. It parses the scope with `ScopeFromQuery` and accepts only its one target.
- **`Clients`** returns `kube.NewBackend` over a `rest.Config` holding only the API
  server's address, its CA and the user's token. Tests that try to get past this boundary
  are krm-foyer's to write; no type check in krm-stream could prove it.
- **`Authorizer`** is `AllowAll`, because every watch is opened as the user, so
  Kubernetes RBAC decides.
- **When the session ends,** krm-foyer cancels the request context. That aborts the
  stream and cancels the upstream watch; the managed connector then gets a 401 and stops.

Shared watches come later. Everything below is about per-user mode unless it says
otherwise.

## Ask 1: map Kubernetes errors, and recover from them with backoff

**The problem.** `kube.Backend.Watch` wraps any error from the API server (except the
`sendInitialEvents` refusal) in `fmt.Errorf`. The gateway turns any error that is not a
`*StreamError` into a terminal `INTERNAL`. So `FORBIDDEN`, `UNAUTHENTICATED` and
`UPSTREAM_UNAVAILABLE` never come from the Kubernetes backend. What a browser receives
from `Handler` with a backend that fails as `kube.Backend` does:

```text
upstream 403:
data: {"seq":1,"type":"error","code":"INTERNAL","message":"krm-stream/kube: streaming list for https://10.43.0.1:443/apis/hello.krm-foyer.example/v1/namespaces/hello/notes: notes.hello.krm-foyer.example is forbidden: User \"oidc:carol@example.com\" cannot watch resource \"notes\"","terminal":true}

API server unreachable:
data: {"seq":1,"type":"error","code":"INTERNAL","message":"krm-stream/kube: streaming list for https://10.43.0.1:443/api/v1/namespaces/x/configmaps: dial tcp 10.43.0.1:443: connect: connection refused","terminal":true}
```

A user without `watch` permission sees "INTERNAL" where Kubernetes said why. A restart of
the API server ends every open stream permanently, though the spec calls that case
retryable.

**Mapping alone is not enough.** The stream loop answers every non-terminal
`*StreamError` by starting the next cycle at once (`StreamProjection` in
`gateway/stream.go`). It emits a generic `RESYNC_REQUIRED` in place of the error, so the
code and any `retryAfterMs` are lost, and it does not wait. Mapping an unreachable API
server to a non-terminal `UPSTREAM_UNAVAILABLE` would therefore turn an outage into a
tight loop of reconnect attempts. The mapping and the recovery have to change together.

What we would expect, by the spec's own table (§4.3):

| Upstream answer | Today | Expected |
| --- | --- | --- |
| 403 Forbidden | `INTERNAL`, terminal | `FORBIDDEN`, terminal, with the API server's `Status.message` |
| 401 Unauthorized (token expired or rejected) | `INTERNAL`, terminal | `UNAUTHENTICATED`, terminal |
| 404 (resource not served, wrong version) | `INTERNAL`, terminal | `SCOPE_INVALID`, terminal |
| Connection refused, timeout, 5xx | `INTERNAL`, terminal | `UPSTREAM_UNAVAILABLE`, not terminal, with backoff |
| 429 from Priority and Fairness | `INTERNAL`, terminal | `UPSTREAM_UNAVAILABLE`, not terminal, `retryAfterMs` from `Retry-After` |
| 410 Gone / `Expired` on an open watch | `RESYNC_REQUIRED` | Unchanged: a new snapshot cycle |
| Any other `ERROR` event on an open watch | `RESYNC_REQUIRED` | Classified as in the rows above; 401 and 403 terminal |

The mapping is needed on every path an error can take: opening a streaming list, the
LIST and the WATCH of the list-then-watch fallback, and `ERROR` events on an established
watch (`watchError` in `gateway/kube/backend.go` turns all of these into
`RESYNC_REQUIRED` today).

**Who retries.** The spec leaves open whether a retryable error is retried by the gateway,
on the same connection, or by the client, on a new one. We would prefer the client: the
gateway sends the non-terminal error, with its code and `retryAfterMs`, then closes the
connection. Our reasons:

- **One retry policy, not two.** The managed connector already has a bounded budget,
  exponential backoff and jitter. A second backoff in the gateway would multiply with
  it.
- **Nothing is held during an outage.** A gateway that retries keeps the browser's
  connection, a goroutine and the host's subscription slot for an outage of unknown
  length. A closed connection costs nothing until the client returns.
- **No continuity is lost.** A retry in the gateway starts a new snapshot cycle, and a
  reconnect asks for a fresh snapshot anyway, so the browser sees the same thing.

If you prefer that the gateway retries, it needs a cancellable backoff that honours the
upstream's `Retry-After`, and it must keep the error's code and hint on the wire rather
than replacing them with `RESYNC_REQUIRED`. Either way, please say which in the spec.

A shared backend's own upstream watch is a different matter: it has no browser to hand
the retry to, so it must retry with backoff itself.

**Tests we would find convincing:**

- An API server that keeps refusing connections: the number of upstream attempts per
  minute is bounded, and the browser sees `UPSTREAM_UNAVAILABLE`, never `INTERNAL`.
- Cancellation during a backoff, or a closed connection during one, ends it at once.
- 401 and 403, at opening and as watch `ERROR` events, are terminal with the right code.
- 410 on an open watch still starts a new cycle.

## Ask 2: keep unexpected error text off the wire

`asStreamError` uses `err.Error()` as the message of any `INTERNAL` it generates, so
whatever an error says reaches the browser. In the output above that is the cluster's
internal address (`10.43.0.1:443`), and, with `NewBackendForConfig`, the configured host,
which `Backend.upstream` says is "never sent to a browser". `Handler` already declines to
pass on a `Principal` error for the same reason.

We suggest:

- **A fixed message for a generated `INTERNAL`,** such as "internal error".
- **Messages the host chose stay.** A `*StreamError` returned by an `Authorizer` or
  `ClientFor` keeps its message, as does the API server's `Status.message` on a 403
  under ask 1. That message is Kubernetes telling the user about their own access, and
  `/k8s` passes it on verbatim too. It is not the same problem as an internal error.
- **A separate diagnostic hook for the raw error,** rather than `Observer`, whose kinds
  deliberately carry no error messages (`gateway/observe.go`). The host decides what to
  log and what to redact. krm-foyer, for one, logs no token or session ID anywhere, and
  "log the full error" would be too broad a default for it.

A test: an error carrying a recognisable secret string, returned from a backend, appears
in the diagnostic hook and nowhere in the response.

## Ask 3: let `Handler` serve a host that delegates resource admission

`ScopePolicy` answers "may anyone stream this kind of thing here?", separately from
the `Authorizer`'s "may this caller see it". That is a legitimate host policy, and the
right default. krm-foyer has a different, equally deliberate one: what its endpoint
exposes is whatever Kubernetes lets the user read, exactly as `/k8s` does. It
promises its users no allowlist.

`ServeStream` already serves that policy: krm-foyer parses the scope with
`ScopeFromQuery`, checks the target and calls the gateway. So this is not a blocker. We
ask for it because `Handler`'s own comment is right: every host that writes this glue by
hand gets four chances to get security-relevant code subtly wrong, and leaving
`ScopePolicy` out should not mean leaving `Handler` out.

**A possible shape.** An explicit opt-in that stands out in review, as `AllowAll` does:

```go
Scopes: gateway.ScopePolicy{
    Targets:   []string{""},
    Resources: gateway.AnyResource{}, // admission is left to the upstream's own authorization
},
```

With it, any scope that `ScopeFromQuery` accepts passes `Validate` if its target is
allowlisted. An omitted namespace means whatever the upstream makes of it: the list of a
cluster-scoped resource, or every namespace for a namespaced one, which RBAC allows only
to users with a cluster-wide grant. The `Authorizer` still runs, so a host can narrow it
there. The target allowlist and the refusal of `server`, `url`, `token` and the like stay
mandatory.

**What it must not promise.** The option should not claim to prove that the stream uses
the caller's credential. A runtime check for `*SharedBackend` would not prove it: a
`kube.Backend` can carry a service account's client, and a wrapper can hide a shared
backend. Binding the stream to the caller's own credential stays the host's obligation,
and the docs should say so where the option is described.

**The spec.** §8 says the server maps the request to "an allowlisted target + GVR", and
conformance item 10 says the scope is allowlisted. "An allowlisted target and a
host-approved GVR" would cover hosts that approve by delegating to the upstream's own
authorization.

## Ask 4: let `Principal` say why it failed

`Handler` answers every `Principal` error with `FORBIDDEN` "not authenticated". A page
treats the two codes differently: `UNAUTHENTICATED` offers sign-in, `FORBIDDEN` says no.
But not every failure means signing in again will help: when a host's session store is
down, the honest answer is "try later".

We suggest honouring a `*StreamError` that `Principal` returns, as the gateway already
does for `Authorizer` and `ClientFor`, so a host can choose `UNAUTHENTICATED`,
`UPSTREAM_UNAVAILABLE` or `FORBIDDEN` with a message of its own. Any other error keeps
today's generic answer, with the code changed to `UNAUTHENTICATED`, which is what
"not authenticated" says.

## Ask 5: honour retry hints at the layer that retries

The managed connector ignores both hints a host can give: `Retry-After` on an HTTP 429,
and `retryAfterMs` on a non-terminal error event. It uses its own backoff for both.

- **On reconnecting,** the connector should wait at least as long as the last hint
  said, within its own cap. krm-foyer's rate bound sends `Retry-After` with the exact
  time until a request would be let through, so retries before then are sure to fail.
- **A non-terminal event on an open connection** should not make the client reconnect:
  under the gateway's current behaviour, recovery follows on the same connection. The hint
  applies only if the connection then ends.
- **If the gateway retries itself** (ask 1), it should honour the upstream's
  `Retry-After` in the same way.

Smaller, and related: on a non-200 answer the connector reports only `stream: HTTP 403`
and discards the body. When the body is a Kubernetes `Status`, as krm-foyer's refusals
are, its `message` would be more useful to show. A bounded read of a JSON `Status`, or a
decoder the host supplies, with today's text as the fallback, would cover it.
