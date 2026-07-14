# KRM resource stream gateway — requirements & design spec

> **Status:** specification, pre-extraction. The **producer** half of the
> [KRM resource stream protocol](../spec/v1.md) v1: a server component that turns a
> Kubernetes watch into a browser-friendly SSE stream of **complete KRM objects** — including live
> **`status`** — and applies edits back to the cluster *as the signed-in user*. Its consumer is any
> protocol client, including the [live KRM reconcile engine](../docs/client-state-model.md). Working name:
> **`krm-stream-gateway`**.
>
> **It serves KRM, and says so.** It is not a generic document-sync service that happens to sit on
> Kubernetes. Streaming real `spec` and real `status`, with real RBAC and real attribution, *is* the
> product. **We want this to be the library you reach for when a frontend needs to watch Kubernetes.**
>
> **Prior art.** This design is lifted from two implementations that live outside this repo (they are
> ConfigButler's, and their lessons — not their code — are what moved here): the gitops-reverser
> **voter** demo, where the pattern was worked out (`docs/kubernetes-watch-sse-gateway.md` as the
> target architecture; `auth-service/coffee_handlers.go` for SSE and `auth-service/kube_client.go` for
> watch, patch, impersonation and CommitRequest), and gitops-api's `cmd/server/console.go`, a second,
> simpler implementation. This spec generalizes both — and goes past them where they stop short:
> neither treats `status` as a first-class product surface, and both predate **streaming lists** (§3),
> which change the primary algorithm.

## The two use cases it must serve

1. **Live status watch (read-only).** Stream an object and render its `status` — conditions, phase,
   replicas — moving in real time as controllers reconcile it. **No save path is involved at all.**
   This is the headline: *watch Kubernetes reconcile*. It is also the traffic-heaviest case, because
   controllers write `status` constantly — so the fan-out, coalescing and backpressure rules (§8) exist
   mostly to serve *this*, not the editing case.
2. **Live spec edit (read-write).** Stream the same object, let the user edit `spec`/`metadata` while
   it changes underneath them (the engine three-way merges), then apply their patch **as them**.

Both are the same stream. A pure status dashboard consumes half this spec and can ignore §7 entirely.

## 1. Why this exists

Kubernetes is a fine live backend for a browser UI — *if* the browser never holds a cluster credential,
never speaks the watch protocol, and never opens its own upstream watch. The gateway is the server-side
piece that makes that true: it authenticates the caller, owns the watch, emits the
[protocol](../spec/v1.md), and channels writes back with the caller's identity so RBAC
and Git attribution both hold. It is independently useful — a read-only status dashboard needs only the
stream half — which is why it is its own component, not folded into the merge engine.

## 2. Scope

**In scope:** authenticating a stream request; authorizing it to a bounded, allowlisted scope; running
the Kubernetes watch (and reusing it across callers); **projecting** objects per a named, declared
policy and emitting the protocol; SSE framing, heartbeats, fan-out, coalescing, backpressure; applying
a save **as the caller** (RBAC + attribution) while refusing to write anything the caller was never
shown; optionally binding the save's *reason* to a Git commit message; operational limits and
observability.

**Non-goals** (from voter §3, and they matter): a raw proxy for arbitrary watch URLs; kubectl parity in
the browser; **exposing Kubernetes bearer tokens to the browser**; supporting every selector combination
in the first cut. The browser asks for a *logical* scope; the server decides what that maps to. It is
not, and must never become, a universal cluster-read API.

## 3. Reaching the API server — the target design assumes modern Kubernetes

> **The environment assumption, stated once and relied on throughout.** This gateway targets **modern
> Kubernetes only** (we pin k3s v1.36 / kcp v0.32). Two consequences drive the whole design:
>
> 1. **Streaming lists are available** for effectively every resource — the API server can synthesize a
>    list as `ADDED` events terminated by a bookmark. That primitive maps **one-to-one** onto the
>    protocol's `reset` … `synced`.
> 2. **`resourceVersion` is a monotonically increasing integer** for a given resource type within a
>    single upstream target (it is the backing store's revision). The gateway relies on this
>    *internally*, and it is worth more than it first appears (§3d).
>
> Neither assumption is exposed on the wire. The protocol stays backend-agnostic (its §5, §6) — which
> is precisely what lets us pick the best backend per deployment, and change it later, without touching
> a single browser.

### 3a. Primary: the streaming list (`WatchList`)

One request does snapshot **and** live tail, with a server-guaranteed handoff:

```go
opts := metav1.ListOptions{
    AllowWatchBookmarks:  true,
    SendInitialEvents:    ptr.To(true),
    ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
    ResourceVersion:      "",          // "" ⇒ start from the freshest state
}
w, err := client.Resource(gvr).Namespace(ns).Watch(ctx, opts)

emit(Reset{Target: t, Scope: k, Projection: p})           // before the first event
for ev := range w.ResultChan() {
    switch {
    case isInitialEventsEndBookmark(ev):                  // metadata.annotations["k8s.io/initial-events-end"] == "true"
        emit(Synced{})                                    // ← the snapshot boundary, straight from the API server
    case ev.Type == watch.Bookmark:
        remember(rvOf(ev))                                // absorbed; never forwarded (protocol §2)
    case ev.Type == watch.Error:
        // 410 Gone / expired RV ⇒ restart this whole block: a new reset…synced cycle
    default:
        emit(project(ev))                                 // added / modified / deleted
    }
}
```

The synthetic `ADDED` events before the bookmark **are** the snapshot; the bookmark **is** `synced`;
everything after it is live. There is no list-then-watch gap to reason about, no `resourceVersion`
to thread by hand, and no full list held in the API server's memory at once.

*This is the algorithm the protocol deliberately does not name.* It lives here because it is one
conforming way to satisfy "gap-free handoff", not the definition of it.

> **Verified** against Kubernetes v1.36.2: the synthetic `ADDED`s arrive, and the snapshot really is
> terminated by a bookmark carrying `k8s.io/initial-events-end: "true"` — an annotation that appears
> **nowhere** in the API documentation, and on which this entire design rests
> ([observed-v1.36.2+k3s1.md](../docs/facts/observed-v1.36.2+k3s1.md), F1).
>
> **But not universal.** The same request is *refused* by an aggregated API server — see §3b, which is
> therefore a required path and not a fallback.

### 3b. list-then-watch at a pinned `resourceVersion` — **not optional**

> ### ❗ This section said "fallback", and a real cluster proved that wrong.
>
> `task cluster-facts` (F6, [observed-v1.36.2+k3s1.md](../docs/facts/observed-v1.36.2+k3s1.md)) pointed
> the streaming list of §3a at a real **aggregated API** — Kubernetes' own sample-apiserver — and the
> request was **refused outright**:
>
> ```
> ListOptions.meta.k8s.io "" is invalid: sendInitialEvents: Forbidden:
>   sendInitialEvents is forbidden for watch unless the WatchList feature gate is enabled
> ```
>
> An aggregated API server is a **separate binary with its own feature gates**. `WatchList` being on in
> kube-apiserver says nothing about it. So this is not a fallback for old clusters — **it is the only
> way to serve an aggregated API on a current one**, and a gateway that implements only §3a cannot open
> a stream for a `Flunder` at all.
>
> **Consequence for the implementation:** open with §3a, and on an `Invalid`/`Forbidden` rejection
> mentioning `sendInitialEvents`, fall back to §3b **for that scope** and remember it. The snapshot
> boundary is then ours to synthesize (`synced` after the last `Added`) rather than the API server's to
> hand us — which is precisely why the protocol names the boundary and not the mechanism.
>
> **This is now built**, in [`kube`](kube/) (`kube.NewBackend`) — a separate module, so client-go stays
> opt-in. It ships both paths and *detects* which one an API needs: nobody should have to know which of
> their APIs is aggregated in order to watch it. The refusal is remembered **per GroupVersion**, since
> it is a fact about a server binary and not about a request. `task test-cluster` drives both paths
> against a real cluster.

What gitops-api's console (`handleConsoleStream`) and voter's config stream
(`streamCoffeeConfigWatch`) ship today, and what a target that cannot do §3a requires:

```go
list, _   := client.List(ctx, opts)                                        // snapshot @ list.resourceVersion
emit(Reset{}); for _, o := range list.Items { emit(Added{project(o)}) }; emit(Synced{})
watcher, _ := client.Watch(ctx, opts{ResourceVersion: list.ResourceVersion})  // resume exactly there — no gap
for ev := range watcher.ResultChan() { emit(project(ev)) }
```

Correct and simple. Its ceiling: one Kubernetes watch **per browser tab**, a full list per reconnect,
and duplicated list/watch/auth work per connection (voter's design doc §1).

### 3c. Scaling: shared informer, fan-out to N subscribers

The target under load (voter design doc §5, §8, §11): **one** `client-go` dynamic informer per
normalized scope key, feeding a **local broadcaster** that N SSE subscribers share.

```text
scope key ─▶ shared informer (streaming-list under the hood; local cache; relist; reconnect)
                 └─▶ broadcaster ─▶ subscriber₁ (bounded chan) ─▶ SSE
                                  ─▶ subscriberₙ (bounded chan) ─▶ SSE
```

A joining subscriber gets its `reset` … `synced` **from the informer's warm cache**, then live deltas —
which is why a reconnect storm is cheap here and expensive in §3a/§3b. Informers already do
list→watch, keep a local cache, and centralize relist/reconnect/`resourceVersion` handling; modern
client-go informers use the streaming list internally.

**A relist or cache reset is not invisible.** It MUST surface downstream as a non-terminal
`RESYNC_REQUIRED` error followed by a fresh `reset` … `synced` cycle **on the still-open SSE
connection** (protocol §5). This is the case the previous draft missed entirely by tying snapshots to
browser reconnects.

**Recommendation:** ship §3a behind the protocol; graduate to §3c when a scope exceeds a few concurrent
subscribers. The browser contract is byte-identical across all three — which is the entire reason the
protocol is specified separately.

> **This is now built**, as `gateway.SharedBackend` — and it is a `Backend` like any other, so it
> wraps whichever of §3a/§3b you are already using and the stream loop cannot tell. Ten tabs on one
> namespace become one upstream watch and one warm cache; a joiner gets its whole `reset`…`synced`
> from that cache without the API server hearing about it.
>
> **It is opt-in, and the reason is not performance.** A shared watch is opened ONCE, so it is opened
> as ONE identity — your service account. Without sharing, `ClientFor` hands the gateway a client
> acting *as the caller*, and Kubernetes' own RBAC is the enforcement: a bug in this library cannot
> show someone objects they may not see. **With sharing, your `Authorizer` is the only thing standing
> between a caller and the objects**, and a bug there is not a bug, it is a disclosure. That is a
> choice about your threat model, so the library will not make it for you — it will only refuse to
> make it silently.
>
> Two properties worth knowing, both tested: a subscriber that falls behind is **resnapshotted from
> the warm cache**, never blocked and never silently starved (a bounded queue, so one paused tab
> cannot grow memory for everyone else); and a partial object is refused *at the cache*, not just at
> the wire — a husk forwarded once blanks one consumer's object, but a husk **cached** is served to
> everyone who joins later.

### 3d. What monotonic `resourceVersion` buys us (internal, never on the wire)

Because `resourceVersion` is a monotonic integer per (target, resource type), the gateway can hold one
`int64` cursor per uid and per scope, and thereby:

| capability | how |
|---|---|
| **Safe coalescing** (protocol §6) | keep only the newest pending event per uid in a subscriber's buffer. A slow tab under status churn collapses 200 events into 1 — *and still converges*, because a lower RV can never legally follow a higher one. This is the single biggest win for the status-watch use case. |
| **Per-object monotonicity** (a protocol guarantee) | drop any event whose RV ≤ the RV already emitted for that uid in this cycle. Cheap assertion, turns a class of ordering bug into a metric. |
| **Idempotent replay after relist** | after an informer relist, an object whose RV the subscriber already saw needs no re-emit beyond the snapshot's own `added`. |
| **Gap detection** | a watch that resumes below the last emitted RV is a bug or a shard change — detect it, `RESYNC_REQUIRED`, re-snapshot. |
| **Cheap "is this stale"** | comparing two versions of one object is an integer compare, not a deep diff. |

**The caveats.** The guarantee holds *within one upstream target and one resource type*. It
does **not** hold across targets (two kcp workspaces, two clusters), across aggregated API servers, or
across a kcp shard migration. So: use it inside the gateway keyed by the normalized scope; never
compare RVs across scope keys; never publish it as an ordering primitive to the browser. The protocol
keeps `resourceVersion` opaque (its §6) for exactly this reason.

## 4. Architecture & package shape

Borrowed from voter design doc §14, the concerns separate cleanly:

```text
api/       HTTP handlers, request parsing, auth integration, SSE endpoint
auth/      authenticate caller; authorize a StreamRequest → an AuthorizedScope (normalized key)
watcher/   backend (streaming-list | list-watch | informer), lifecycle, per-uid RV cursors
project/   named projections: removal + redaction, and the redacted entries they must declare
stream/    SSE framing, broadcaster, per-subscriber lifecycle, coalescing buffers
write/     apply-as-user (patch), redaction guard, and the reason→commit binding
```

The load-bearing separation: **API parses, auth decides, watcher owns Kubernetes, project owns what the
browser may see, stream owns fan-out, write owns attribution.** Each is independently testable; the API
layer never touches a Kubernetes client directly. `project/` is shared by `watcher/` and `write/` — the
same policy object that *removes* a field must be the one that *refuses to write* it (§7).

## 5. Authorization — the browser asks for a scope, the server grants it

The single most important safety rule (voter §7, §16): **the browser must not describe its own
authorization.** It asks for a *logical* scope; the server maps it to an allowlisted target + resource
and the caller's permitted namespace.

```go
// logical, straight from the browser — never trusted
type StreamRequest struct { Target, Resource, Namespace, Name, LabelSelector string }

// normalized, server-decided — the one currency
type ScopeKey struct {
    Target        string   // an allowlisted upstream id — NOT a URL
    Group, Version, Resource string
    Namespace     string   // empty ⇒ the resource is cluster-scoped (NOT "all namespaces")
    Name          string   // empty ⇒ collection scope
    LabelSelector string   // empty unless the gateway allows and validates one
}

type Authorizer interface {
    AuthorizeStream(ctx, principal, StreamRequest) (ScopeKey, Projection, error)  // deny by default
}
```

The normalized `ScopeKey` is used for **authorization, informer reuse, fan-out keying, RV cursors, and
metrics** — one key, five jobs. Note the authorizer returns the **projection** too: what you may see is
an authorization decision, not a rendering preference.

Rules:

- **`Target` is an allowlisted identifier, never a URL.** A gateway that can reach more than one
  cluster/workspace/tenant MUST carry it (protocol §8); one that reaches exactly one MAY hardcode it —
  but the field exists from day one, because retrofitting a target key into an informer cache and a
  uid map afterwards is the kind of change that breaks a released library.
- Allowlist resources explicitly. Keep scopes narrow: one kind, one namespace, optionally one name.
- **Cluster-scoped resources are supported.** Namespaced-with-no-namespace is a *denial*, not a
  wildcard: **all-namespace watches are not in v1** — one namespace or one cluster-scoped resource.
- **Field selectors are not in v1.** A **label selector** MAY be accepted, and if so MUST be validated
  and MUST become part of the `ScopeKey` (so two callers asking the same selector share one informer).
- gitops-api's current gateway hardcodes the scope (ConfigMaps in the caller's resolved namespace) — a
  degenerate authorizer, which is fine. The seam must exist so a second resource, a second workspace,
  or a name-scoped stream doesn't require reworking the handler.

## 6. Identity — reaching the cluster *as the caller*

This is the part that is **half reusable, half deployment-specific**, and the spec must mark the line.

### 6a. Two ways to carry the caller's identity to the API server

**Strategy A — the caller's own token (gitops-api console).** The server holds the caller's OIDC (Dex)
token server-side and presents it as the bearer token on every watch/read/write. The API server sees the
*user*; RBAC and audit attribution follow for free; the server needs **no** impersonation privilege.
Requires the token to be custodied server-side and to be directly presentable to the target API.

**Strategy B — impersonation (voter).** The server authenticates with its *own* ServiceAccount and sets
`Impersonate-User` / `Impersonate-Group` (+ claim extras) to the caller (`impersonatedDynamic` in
voter). The API server records the impersonated user. Requires the server to hold a privileged
credential and to have `impersonate` RBAC on `users`/`groups` **and** on the specific `userextras/<key>`
it forges (the display-name/email extras the reverser reads as the Git author — gitops-api learned this
one the hard way; see its operator RBAC).

| | A: caller's token | B: impersonation |
|---|---|---|
| server privilege | none (acts only as the user) | high (can impersonate anyone) |
| RBAC needed | none extra | `impersonate users`,`groups`,`userextras/<claim-key>` |
| attribution | native (it *is* the user) | via impersonated user + claim extras |
| needs | token presentable to target API | privileged cred + a way to know the user |
| fits | user has a bearer token for the target (Dex→kcp workspace) | server has cluster creds; user has no directly-usable token |

Both are valid; a gateway MUST support at least one and name which. **Strategy A is the safer default**
where a user token exists (the server never becomes a privileged actor). Impersonation is the answer
when writes must be attributed but the user has no token the target API accepts directly.

> **Identity and informer sharing are in tension — say which you chose.** A shared informer (§3c) runs on
> *one* credential, so N subscribers with different RBAC share one upstream read. That is only sound if
> **authorization is enforced at the scope boundary** (the authorizer already decided this principal may
> see this whole scope) and the scope is exactly what the informer caches. Never fan a shared informer
> out to subscribers whose authorization differs *within* the scope. If per-object authorization is ever
> needed, the informer must be per-principal — i.e. you are back to §3a/§3b. v1: **scope-level authz,
> shared informer.** Write it down; it is the assumption most likely to be violated by a later "small"
> feature.

### 6b. Authenticating the stream request itself

Native `EventSource` cannot send an `Authorization` header (protocol §7). Therefore:

- The gateway's **baseline** is a **same-origin session cookie** (`HttpOnly`, `Secure`, `SameSite`) —
  which is what gitops-api already has from the Dex login, and what the console uses today.
- It MAY *additionally* accept `Authorization: Bearer` for non-browser consumers and for browser
  consumers using a fetch-based SSE reader.
- It MAY accept a **short-lived, single-scope stream ticket** as a query parameter for cross-origin
  browsers. **A long-lived credential MUST NOT be accepted in the URL** — it would end up in proxy
  logs, browser history and `Referer` headers.

### 6c. The network hop (deployment-specific)

gitops-api reaches the kcp workspace API through the in-cluster front-proxy Service, TLS verification
skipped — the same hop Traefik already makes for the browser's `/kcp` path (its `insecureSkipVerify`
ServersTransport). This is a gitops-api/kcp deployment detail, **not** part of the reusable gateway: the
reusable core takes a `rest.Config` (or a factory keyed by `(Target, principal)`) and does not care how
it was built. Mark it clearly so the extracted library carries no kcp assumptions.

## 7. Projection & the write path — never write back what you never showed

### 7a. Projections are declared, named, and machine-readable

The gateway does **not** get an unspecified field-dropping policy. Each projection is a named object
that declares exactly what it removes and what it redacts, and it emits `redacted` (RFC 6901 JSON
Pointers) per object so a consumer can tell "absent" from "hidden" from "masked" (protocol §3).

| projection | removes | redacts |
|---|---|---|
| `krm-raw/v1` | `metadata.managedFields`, `kubectl.kubernetes.io/last-applied-configuration` | — |
| `krm-full/v1` | the above | Secret values, by default |
| `krm-spec/v1` | the above plus `status` | Secret values, by default |

Everything else is carried **verbatim** — all of `spec`, `data`, `type`, and any
root field a CRD invents. The projection exists so a human editor is never shown machinery; it must
never be so aggressive that it prunes substance. (Both prior examples do exactly this much:
gitops-api's `configMapView`, voter's `toCoffeeConfig`.)

**`status` is a first-class product surface when a projection sends it.** `krm-full/v1` keeps it
read-only and live; `krm-spec/v1` ignores it and suppresses status-only updates. Not every resource
has a `status` (`ConfigMap`, `Secret`), and projections never synthesize one.

### 7b. Secrets — the gateway must choose a disclosure policy

`Secret.data` is base64-encoded **sensitive** material. Streaming it to a browser is a deliberate
decision, never a default, and it is **the gateway's** decision — the protocol just carries the result
plus the `redacted` entries that make it safe. Pick one, per kind or per scope, and make it explicit:

| policy | when | on the wire |
|---|---|---|
| **keys only** (values omitted) | **default.** The UI shows *what* keys exist and can edit labels/annotations, without disclosing values. | value **deleted**, `{path, rev}` in `redacted` |
| **elevated auth** (values only for an authorized principal / re-auth) | operator consoles. | full value, path absent from `redacted` |
| **full values** | trusted, narrowly-scoped operator UI only — and say so loudly. | full value, empty `redacted` |

> **There used to be a fourth policy here — "masked": put `"••••"` in the value.** It is gone, and
> spec §3.1 now **forbids** it. A placeholder in the object is a value a browser can **save back**, and
> a merge patch carrying `"••••"` writes that literal string **over the real Secret**. It was the only
> poisoned value in the system, and we invented it (proposal 0003).
>
> Nothing was lost by deleting it: `redacted` is mandatory and authoritative, so it already
> carries the one thing the mask carried — that the key exists and is withheld. **A mask is something
> a UI draws. It is not something the wire carries.**

`redacted` is therefore the *only* evidence of redaction, which is exactly why it is mandatory
and not "nice for debugging": a real value that merely *looks* like a mask is not redacted, and
redaction is never inferred from a value's shape.

Independently: `binaryData` and any non-UTF-8 value are streamed as opaque (`"N bytes"`), never as
editable text, so a text input cannot corrupt them.

### 7c. The write path

1. **Apply a constrained patch as the caller.** Take the object identity + a **JSON merge patch**
   (RFC 7386, built by the consumer — the reconcile engine's `patch(id)`) and `PATCH` it with the
   caller's identity (Strategy A or B). RBAC gates it; a forbidden write `403`s; a conflict `409`s and
   is surfaced. gitops-api does exactly this (`handleConsoleSave` → `ConfigMaps().Patch(MergePatchType)`);
   voter the same via `patchCoffeeConfig`.

2. **The redaction guard — non-negotiable.** Before the patch leaves the gateway, it is validated
   against the **same projection object** that produced the stream:
   - a patch touching any path in that object's `redacted` → **reject** (`400`), never write;
   - a patch touching any path the projection *removed* → **reject**;
   - a whole-object `PUT` → **not supported at all**. A projected object must never be sent back as if
     it were complete: the omitted fields would be interpreted as deletions.

   This is why `project/` is a package and not a function in `watcher/`: the removal rule and the
   write-refusal rule are one policy, and they must not be able to drift.

3. **Bind the reason to the Git commit (ConfigButler-specific, optional).** The editor collects a
   *reason* — a commit message. voter turns it into one by creating a ConfigButler **`CommitRequest`**
   CR right after the patch, under the *same* impersonated identity, with `spec.message` = the reason
   (`createCommitRequest`); an empty reason is left off so gitops-reverser falls back to its generated
   grouped-commit message. gitops-api **defers** this — its save records the reason in a log only —
   pending the reverser-side work. Treat the reason as a first-class input and the CommitRequest as a
   pluggable **commit finalizer**, so a non-ConfigButler deployment can omit it and a ConfigButler one
   can wire it.

   > Ordering matters: the finalizer runs *after* the write, as the *same* identity, so the reverser
   > binds the finalize signal to the same audit actor. Keep them on one code path.

*(Open, §12: server-side apply as an alternative to merge patch — it handles associative lists properly
and gives real field ownership, at the cost of needing a `fieldManager` identity per user.)*

## 8. Fan-out, coalescing, backpressure & limits

Per voter §11–§12, plus what monotonic RVs (§3d) make possible — the rules that keep one gateway from
taking down an API server, and one slow tab from stalling everyone:

- **One broadcaster per `ScopeKey`.** It registers/unregisters subscribers, serves the snapshot on
  join, and fans out deltas.
- **Bounded per-subscriber channels** (voter sizes its order channel at 16).
- **Coalesce, don't just drop.** Under status churn the right behaviour is not "buffer 200 events" and
  not "disconnect the tab" — it is **keep only the newest event per uid** in the buffer. Monotonic RVs
  make this provably convergent (§3d), and it is the difference between a status dashboard that scales
  and one that doesn't. Deletions are not coalesced away; a `deleted` supersedes any pending upsert for
  that uid.
- **Then, and only then, drop.** If a subscriber's *coalesced* buffer still stays full, disconnect it
  with a terminal `SLOW_CONSUMER` error. It reconnects and re-snapshots cheaply from the warm cache.
- **Limits:** max distinct active scope keys; max subscribers per key and total; idle-informer TTL (keep
  warm briefly after the last subscriber, then stop); per-principal / per-IP stream-creation rate limits.
- **Observability:** active informers, active subscribers, relist count, upstream watch restarts,
  **events coalesced**, dropped (slow) subscribers, authorization denials, redaction-guard rejections,
  per-key fan-out counts, snapshot cycles emitted per stream.

## 9. Failure handling

- **Upstream continuity lost** (`410 Gone` / expired RV / informer relist / cache reset) — emit a
  non-terminal `RESYNC_REQUIRED` error and **begin a new `reset` … `synced` cycle on the same SSE
  connection** (protocol §5). Do not close the browser's connection to achieve a resync.
- **Auth expiry mid-stream** — authorize on connect; on a long-lived stream, re-check when the security
  model demands it. On loss of authorization emit a **terminal** `FORBIDDEN`/`UNAUTHENTICATED` and close;
  the client must not silently keep a stale view.
- **Scope re-authorization** (the caller's namespace/RBAC changed) — treat as a continuity loss:
  `RESYNC_REQUIRED` + a new cycle, with a possibly different `projection` on the new `reset`.
- **Untrustworthy deletion** — if an informer deletion tombstone yields no reliable `uid`, do **not**
  emit an ambiguous `deleted`; force a new snapshot cycle (protocol §4.2).
- **Reconnect storms** — cheap under §3c (warm cache); §3a/§3b pay a fresh streaming-list per reconnect,
  which is the main reason to graduate.
- **Save conflicts** — a merge patch touches only changed keys, so lost-update risk is bounded to those
  keys; surface `409`/`403` to the consumer, which already shows live conflicts pre-save.

## 10. What's reusable vs gitops-api-specific

The extracted library is the left column; the right column stays in gitops-api.

| Reusable core | gitops-api / ConfigButler-specific |
|---|---|
| protocol v1 emission, SSE framing, heartbeat, snapshot cycles | the `/kcp` front-proxy hop + `insecureSkipVerify` transport |
| all three backends (§3a/b/c), RV cursors, coalescing, backpressure, limits | resolving *which* workspace/namespace is the caller's (Tenant/RepoSelection lookup) |
| `Authorizer` seam (allowlist + normalize + choose projection) | the concrete authorizer (owner → their namespace) |
| named projections + the redaction guard on the write path | the Secret policy *choice* for this product |
| apply-as-user, both identity strategies | Dex as the specific token issuer; the kcp workspace as target |
| target registry (allowlisted upstream ids) | the CommitRequest finalizer (ConfigButler CRD) + reverser attribution extras |

Rule of thumb: the core knows *"a target, a scope, a projection, a way to get a client for an identity,
and a broadcaster."* Everything about *how identity, cluster, and Git attribution are wired* is injected.

## 11. Failure-mode / test matrix

A fake watch/informer + a fake API suffice for most of these.

| # | Situation | Expected |
|---|---|---|
| **Protocol framing** |
| 1 | connect (collection) | `reset`, one `added` per object, `synced`, then live deltas |
| 2 | connect (named, object exists) | `reset`, one `added`, `synced` |
| 3 | connect (named, object **absent**) | `reset`, `synced` — **not** an empty stream, and not an error |
| 4 | object deleted while the browser was disconnected | reconnect's snapshot omits it; the consumer prunes on `synced`; no ghost |
| 5 | upstream continuity lost, SSE connection still healthy (relist / `410`) | `RESYNC_REQUIRED` (non-terminal) then a fresh `reset`…`synced` **on the same connection** |
| 6 | connection drops between `reset` and `synced` | consumer prunes nothing; pre-`reset` state survives |
| 7 | watch delivers `BOOKMARK` / metadata-only / `ERROR` frame | absorbed; never forwarded as `added`/`modified` |
| 8 | informer deletion tombstone with no reliable uid | **no `deleted` emitted**; a new snapshot cycle instead |
| 9 | delete + recreate under the same name | old uid pruned, new uid added; consumer state does not bleed across |
| **Backend equivalence** |
| 10 | one randomized op sequence, run through streaming-list, list-then-watch, and informer fan-out | **identical** final projected consumer state in all three |
| 11 | N subscribers, one scope (informer form) | exactly one upstream watch; all N converge |
| 12 | high-frequency status churn, N subscribers | informer form opens **one** upstream watch; naive form opens N (assert the difference — it is why §3c exists) |
| 13 | slow subscriber under churn | events **coalesced** per uid (assert the count collapses); only then, if still full, dropped with terminal `SLOW_CONSUMER`; others unaffected |
| 14 | an event arrives with an RV ≤ one already emitted for that uid | dropped, not emitted (per-object monotonicity) |
| **KRM fidelity** |
| 15 | controller writes **`status` only** (spec unchanged) | a `modified` carrying the new `status`; `spec` byte-identical. *The status-watch use case is this test.* |
| 16 | an **editing** client is connected | it still receives `status` — never omitted for "editors" |
| 17 | a CRD with deeply nested / unknown `spec`, `status`, and a **custom root field** | carried **verbatim**: no flattening, reordering, coercion, schema-check, or dropping of the unknown root field (assert JSON round-trip equality) |
| 18 | a resource with **no `status`** (`ConfigMap`) | no `status` synthesized; object streamed as-is |
| 19 | object carries `managedFields` + last-applied annotation | removed by the projection; `redacted` unaffected (removal ≠ redaction) |
| **Projection, redaction & the write guard** |
| 20 | `Secret` under **keys-only** policy | keys present, values absent, each value path listed in `redacted`; labels/annotations still editable |
| 21 | save patches a Secret value the caller was **never shown** | **rejected** (`400`); no write reaches the API server |
| 22 | save patches a field the **projection removed** (e.g. `managedFields`) | **rejected**; no write |
| 23 | save with a whole projected object as a `PUT` | **unsupported** — the endpoint accepts patches only |
| 24 | a real value that happens to look like a mask (`"••••••"`) | **not** treated as redacted — redaction is decided by the KIND and declared in `redacted`, never inferred from a value's shape |
| 25 | `ConfigMap.binaryData` / non-UTF-8 value | streamed opaque (`N bytes`), never as editable text |
| **Authorization & lifecycle** |
| 26 | unauthorized scope request | denied before any watch opens |
| 27 | non-allowlisted resource, or a raw URL in the request | denied; not proxied |
| 28 | all-namespaces or field-selector request (v1) | denied — not silently widened |
| 29 | two callers, same scope, different principals | one informer; both authorized **at the scope boundary** (§6a) |
| 30 | browser disconnects | watch/subscription torn down; no goroutine leak (assert on `ctx.Done()`) |
| 31 | auth token expires mid-stream | terminal `UNAUTHENTICATED`; connection closed |
| **Write path** |
| 32 | save as caller, allowed | `PATCH` succeeds; audit names the caller; (finalizer) commit authored by them |
| 33 | save as caller, forbidden | `403` surfaced; no write |
| 34 | save with a reason | reason reaches the commit finalizer (or is logged, if deferred); empty → default message |

## 12. Open questions for the new repo

- **Backend order.** Ship §3a (streaming list) first, graduate to §3c (informer fan-out) under load?
  (Recommend yes — the protocol makes it a drop-in, and §3b becomes a compatibility fallback only.)
- **Patch format:** RFC 7386 merge patch (simple; arrays whole; matches the engine's `patch(id)`) vs
  **server-side apply** (correct associative-list merging, real field ownership, natural conflict
  detection) — SSA needs a `fieldManager` per user and changes the engine's patch builder. Decide per
  target API capability; SSA is the more likely long-term answer for a *KRM* library.
- **Identity strategy** as a pluggable interface (`ClientFor(target, principal) rest.Config`) so A
  (token) and B (impersonation) are both just implementations.
- **The commit finalizer** as a pluggable post-write hook (ConfigButler `CommitRequest`, or nothing).
- **Typed vs dynamic client.** **Dynamic (unstructured), decided** — a KRM gateway that only works for
  kinds it was compiled against is not a KRM gateway. `spec`/`status`/custom root fields are free-form
  JSON precisely so any CRD works on day one.
- **Secret disclosure default.** **Keys-only**, decided; anything more permissive is an explicit,
  per-scope opt-in.
- **Status subresource writes.** Default: `status` is read-only. Expose `/status` writes for an operator
  tool? (Recommend: not in v1.)
- **Label selectors:** allowed but gateway-validated (§5). Do we expose a *predeclared* selector per
  scope (safe) or accept an arbitrary one (needs cardinality limits on the informer cache)?
- **Multi-target from day one?** The `Target` field is in `ScopeKey` from v1 even where it is hardcoded
  — retrofitting it into a released library's uid maps and informer cache is the change that breaks
  everyone.
- **Name**, and whether gateway + engine + protocol are three packages in one repo or three repos.
