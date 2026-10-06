# Could `/k8s` share watches without anyone noticing?

**Investigation, 2026-10-06, revised the same day.** Kubernetes facts are from the
Kubernetes repository at `35fc3af1380` (`v1.38.0-alpha.1-235`, checked out in
`external/kubernetes`) and from the `k8s.io/apiserver` and `k8s.io/apimachinery`
v0.36.0 modules krm-foyer already builds against. Paths are relative to
`staging/src/k8s.io/` unless they start with `pkg/`. Nothing here is implemented.

The first version of this document said a transparent shared `/k8s` was not
workable, because a proxy cannot compare resource versions. That was wrong for
watches: Kubernetes now ships a comparison, and Kubernetes' own client-go uses it.
This version replaces that analysis.

## The question

A room of 200 people scans a QR code and opens the same page. Each browser LISTs the
notes and then WATCHes them through `/k8s`, so the API server holds 200 watches of one
collection. krm-stream's gateway avoids that by serving everyone from one upstream
watch, but it does so with its own SSE protocol, which is one more way to do things.

Could `/k8s` notice that many people make the same request, open **one** watch, and
send each of them the same data, without any client being able to tell? Is that not
the same game as the gateway's shared watches?

## Short answer

- **For WATCH, yes, soundly**, on resources whose resource versions are integers
  (everything stored in etcd: built-in resources and CRDs). One upstream watch per
  `(resource, namespace, selectors, encoding)` can serve every request that watches
  from a resource version at or after the point where the shared watch started. Each
  client gets exactly the events after its own version, the same events the API server
  would have sent it.
- **For LIST, it is not worth it.** The API server already serves consistent LISTs
  from its cache, and people in a room arrive over tens of seconds, not milliseconds.
  LISTs stay pass-through.
- **Visible to an operator, not to a client.** The API server's audit log shows the
  shared watch and a SelfSubjectAccessReview per user, instead of a watch per user.
  krm-foyer must also start telling watches apart, using the API server's own parser.
- **It is the same game as the gateway's sharing, minus the protocol.** The gateway
  can admit a subscriber at any moment because its protocol defines what a subscriber
  gets: a snapshot from the gateway's cache. The native protocol defines freshness
  relative to the client's own request, so the proxy may only serve what the API
  server would have served. Comparable resource versions are what make that check
  possible.
- **It reduces the number of ways.** Pages use one browser connector, krm-stream's
  native one, against `/k8s`. Whether a resource is shared becomes a deployment
  setting, the same `-shared-watch-resources` that exists today, not a choice each page
  makes. The gateway remains for pages that need a projected view (redaction,
  suppression).

**Recommendation:** measure the room first (an experiment is
[below](#measure-first)). If the watches hurt, build this in `/k8s` rather than asking
pages to switch protocol.

## Why a watch, not a LIST, is the thing to share

### What Kubernetes already shares

| Mechanism | What it does | Where |
| --- | --- | --- |
| **Watch cache (`Cacher`)** | "Responsible for serving WATCH and LIST requests for a given resource from its internal cache." One storage watch per resource, however many clients watch it | `apiserver/pkg/storage/cacher/cacher.go` |
| **`cachingObject`** | "Able to cache its serializations so that each of those is computed exactly once": a thousand JSON watchers of one event cost one JSON encoding | `apiserver/pkg/storage/cacher/caching_object.go` |
| **Consistent LIST from cache** | A LIST without a `resourceVersion` is served from the watch cache, which first proves it is fresh with an etcd progress notification (`RequestWatchProgress`). GA and locked on since 1.34 | `apiserver/pkg/features/kube_features.go` (`ConsistentListFromCache`) |
| **Watch history** | At least `DefaultEventFreshDuration` (bookmark frequency, one minute, plus 15 s), from 100 up to 102 400 events | `apiserver/pkg/storage/cacher/watch_cache.go` |

A LIST is therefore a read from memory, charged by APF (API Priority and Fairness, the
API server's request throttling) by object count, and then it is over. Two hundred
LISTs of a small collection spread over a minute are not a storm.

### What a watch keeps costing

Per open watch, the API server keeps a watcher with its own goroutine and buffer
(`cache_watcher.go`). It filters every event of the resource for that watcher and writes
it to that connection. A watcher that cannot keep up is terminated: "Since we don't want
to block on it infinitely, we simply terminate it."

APF makes the cost land on writers: `apiserver/pkg/util/flowcontrol/request/mutating_work_estimator.go`
charges every write `ceil(watchers / 10)` extra seats for 5 ms once its resource has
10 or more watchers (`watchesPerSeat = 10`). Its comment: "the actual work associated
with processing watch events is happening in multiple goroutines (proportional to the
number of watchers)".

So 1800 watches on one resource (the size of krm-foyer's
[rehearsal](../bounds.md#measured-the-rehearsal)) make every write to it cost about
180 seats for 5 ms. That counts against the writer's priority level, which can be a
controller's. One shared watch makes it one watcher. **That is the load worth removing**,
and it comes from watches, not LISTs.

## How a shared native watch stays exact

### The missing piece: comparable resource versions

The API documentation tells clients to treat `resourceVersion` as opaque. The API server
keeps its cache fresh by comparing versions internally. A proxy that cannot compare
cannot know whether a client's version falls inside the history it holds.

Kubernetes has since added `apimachinery/pkg/util/resourceversion.CompareResourceVersion`
(commit `2cef54c1450`, 2025-09-29, "Add helper function for client-go to compare
resource version"). It is present in v0.36.0, which krm-foyer already depends on:

> CompareResourceVersion runs a comparison between two ResourceVersions. This only has
> semantic meaning when the comparison is done on two objects of the same resource.
> ... The function will return an error if the resource version is not a properly
> formatted positive integer.

Kubernetes uses it the same way a proxy would. `client-go/util/consistency` tracks
"written resource versions and ensuring that local informer caches have observed
resource versions at least as new as those written". The storage version migrator also
relies on it and fails a migration when versions are not comparable
(`pkg/controller/storageversionmigrator`). Where a version is not a well-formed integer,
as an aggregated API may serve, the helper returns an error. The proxy then simply
passes the request through.

### The rule

The proxy holds one upstream watch per key: resource, namespace, label selector, field
selector, `Accept` encoding. It opened that watch from version **S** and keeps a history
of every event since, in order.

A user's `WATCH ?resourceVersion=X` for that key can join if `X ≥ S`. The proxy then
sends that user exactly the events with version **> X**: first from history, then live.
This is what the API server would have sent. A watch from X means "everything after X",
and the shared watch has received everything after S, in order, for the same key.
Events at or before X are already in the user's LIST. An event that arrives later with a
version ≤ X is skipped, for the same reason.

There is no freshness wait. If the user's X is ahead of what the shared watch has
received so far, the missing events are still on their way, in order, and arrive like
any live event.

Everything else passes through to the API server, as today: a LIST; a WATCH with
`X < S`, an unset or `"0"` version, `sendInitialEvents`, or a non-integer version; a
resource that is not configured for sharing.

For the room: the first person's WATCH opens the shared watch from their LIST's
version. Everyone who arrives later has a LIST at least as new, so `X ≥ S`, and joins.
krm-stream's native connector resumes from the last version it applied, which on a
shared stream is a version the shared watch delivered, so that joins too. Two hundred
browsers, one watch at the API server.

### What else must match

| Native behaviour | Shared `/k8s` | Source |
| --- | --- | --- |
| Recognizing a WATCH (`watch=1`, `watch=yes`, a bare `watch`, `/watch/` paths) | Use the API server's own `RequestInfoFactory`, not a second parser ([why krm-foyer avoided this so far](../bounds.md#native-watches)) | `apiserver/pkg/endpoints/request/requestinfo.go` |
| Authorization of the user's WATCH | A SelfSubjectAccessReview **with the user's own token**, built from the same attributes the API server's authorization filter builds: verb, group, version, resource, namespace, name (from a `metadata.name` field selector), field and label selector requirements. Kubernetes keeps the SAR fields and the authorizer attributes in step with a test (`pkg/registry/authorization/util/helpers_test.go`). No `create subjectaccessreviews` grant is needed for this | `apiserver/pkg/endpoints/filters/authorization.go` (`GetAuthorizerAttributes`) |
| Authentication | The same SelfSubjectAccessReview only succeeds with a token the API server accepts | |
| A response that depends on who asks | None for list and watch in kube-apiserver's registries: their uses of the requesting user are on create and update (CSR requestor, PodCertificateRequest status, RBAC escalation, admission policy bindings, the self-reviews). Aggregated APIs may filter per user, so pass them through | `pkg/registry/**` (`UserFrom`) |
| Event bytes | Same key, same `Accept`: the frames can be relayed as received, parsed only for type and version | |
| Bookmarks | Relay those with a version > X when the client asked for bookmarks | |
| Object leaving a label selector | Keyed on the exact selector, so the API server already turns it into `DELETED` | |
| History expired (410) on the shared watch | Every subscriber receives the 410 and re-lists, as each would have | |
| Shared watch ends (API server timeout) | Reopen from the last version, unseen; the history continues | |
| Per-client `timeoutSeconds`, response bounds, logout | Applied per client, exactly as today; ending one client's response leaves the others | `internal/gate` |
| Slow client | Drop that client alone (abort). The native connector resumes and joins again from its version, as the API server's own watch cache does with a slow watcher | `cache_watcher.go` |
| Revocation | A native watch is authorized once and never rechecked. Rechecking like shared streams do is optional and stricter | |
| Audit | The API server records the shared identity's watch and each user's SelfSubjectAccessReview, not each user's watch | **Visible to operators** |

The identity that holds the shared watch is the one shared watches already use:
`-shared-watch-token-file`, granted `list` and `watch` on the configured resources and
nothing else. With per-user SelfSubjectAccessReviews it no longer needs
`create subjectaccessreviews`. That part of the existing gateway path could be
simplified the same way.

### What it costs krm-foyer

- **Telling watches apart.** `bounds.md` records the decision not to parse requests.
  This would reverse it for configured resources only, using the API server's own code.
  That adds a dependency on `k8s.io/apiserver`, a large module, and krm-foyer must keep
  it in step with the clusters it fronts.
- **A history per shared watch,** bounded in events and age like the API server's. A
  client older than the history passes through, so the only effect is less sharing.
- **Two code paths in `/k8s`:** pass-through and shared. Every bound, the logout cut-off
  and the response limits must hold on both, tested against the real API server.
- **Security tests that try to get through:** a user denied by RBAC joins nothing; a
  shared watch never delivers to a session after logout; one user's slow or aborted
  stream never stalls another's; a client with `X < S` never receives a partial history;
  a non-integer version, an aggregated API and an unconfigured resource always pass
  through; the shared identity is never used for anything but the watch.

## Is it the same game as the gateway?

| | Gateway shared stream | Shared native watch in `/k8s` |
| --- | --- | --- |
| Upstream | One watch per scope | One watch per key |
| A subscriber can join | Any time: the gateway sends a snapshot from its cache | When its version ≥ S: the proxy sends what the API server would have |
| Freshness defined by | krm-stream's protocol | The Kubernetes API |
| Browser code | `connectResourceStream` (SSE) | `connectNativeWatch`, unchanged |
| Per-user authorization | SubjectAccessReview by the shared identity, rechecked | SelfSubjectAccessReview with the user's token, at join |
| Projection, redaction, suppression | Yes | No: native objects |
| Reconnect | Fresh snapshot | Resume from the client's version |

The fan-out is the same. What differs is who defines what a joining client gets. The
gateway can take any subscriber because it owns its protocol. The proxy must stay inside
the native one, so it may only take a request whose answer it can reproduce exactly.

## Prior art

| Project | Shares watches across users? | API it serves |
| --- | --- | --- |
| kube-apiserver `Cacher` | Yes: one storage watch per resource | Native. It owns consistency with etcd |
| client-go `SharedInformerFactory` | Within one process and one identity | Go objects in memory |
| client-go `util/consistency` | No, but it is the same "has my cache caught up" check, by comparing resource versions | |
| `kubectl proxy` | No | Native, passed through |
| [Rancher steve](https://github.com/rancher/steve) | Yes: its own informers, with per-user access computed from RBAC bindings | Its own WebSocket watch API, `/v1/subscribe`; its Kubernetes proxy passes through with `Impersonate-*` |
| krm-stream gateway | Yes, `SharedBackend` | Its own SSE protocol |

I found no proxy that shares behind the native API. The ones that share define their own
protocol. Comparable resource versions are recent (2025), which may be part of why.
This is not a survey of every dashboard.

## Fewer ways, not more

Today a page chooses between three: a per-user stream, a shared stream (both via the
gateway's SSE) and a native watch. With shared native watches the choice becomes:

1. **Does the page need a projected view** (Secret values withheld, status-only updates
   suppressed)? Then the gateway.
2. **Otherwise, native.** One connector, `/k8s`, and whether the resource is shared is
   the operator's setting, not the page's.

Before choosing, also ask whether the gateway's per-user streams and shared streams are
still needed once native sharing exists. The gateway would keep projected views; its
sharing would serve only pages that use them.

## Measure first

Add a native variant to the rehearsal (`test/e2e`), next to its per-user and shared
gateway runs:

- 200 identities, each with krm-stream's native connector on one collection through
  `/k8s`, and again on nine collections;
- a steady write rate to the watched resource from a controller-like client in its own
  APF priority level;
- recorded: `apiserver_registered_watchers`, `apiserver_flowcontrol_work_estimated_seats`
  and wait time for those writes, write latency, API server CPU and memory.

If 1800 native watches do not visibly delay writes or grow the API server, nothing
needs sharing. If they do, the same run is the baseline for the shared `/k8s`, which
should show one watcher and unchanged write cost.

## Side finding: selectors in shared-watch reviews

krm-stream's `kube.SubjectAccessReviewAuthorizer` leaves label and field selectors out
of its reviews (`gateway/kube/authz.go`), and krm-foyer's decision cache keys on the
scope without the label selector ([watches](../watches.md#what-happens-for-each-stream)).
Since `AuthorizeWithSelectors` went GA in 1.34, an authorizer (a webhook, not RBAC) may
allow a list or watch only with a certain selector. Asking without the selector asks the
broader question, so such a grant is refused. That is the safe direction: it can
over-refuse, never over-grant. A shared `/k8s` would send the selectors, as above.
