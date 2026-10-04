# krm-stream feedback from krm-foyer: shared watches

krm-foyer now hosts `SharedBackend` with `kube.SubjectAccessReviewAuthorizer`
([roadmap step 5b](https://github.com/ConfigButler/krm-foyer/blob/main/docs/roadmap.md#order-of-work), [watches](https://github.com/ConfigButler/krm-foyer/blob/main/docs/watches.md)). This note asks
for five changes, numbered on from the [first feedback](first-fieldreport.md), whose
asks 1 to 8 krm-stream 0.5.0 and 0.6.0 took. Two (9 and 10) are about guarantees that
do not hold today in a failure case; three (11 to 13) would cut the authorization load
that sharing costs. For each we say what krm-foyer does meanwhile. Everything was
checked against krm-stream main at `5ca19ed` (gateway 0.6.0) on 2026-10-04, by reading
the source and by tests in krm-foyer that reproduce each problem.

## Status (2026-10-04): asks 9, 10 and 13 are taken

[Proposal 0008](../proposals/0008-shared-watch-hardening.md) selected ask 9, the "refuse the
combination" form of ask 10, and the documentation part of ask 13:

- **Ask 9:** a shared scope now opens its upstream watch without the backend-wide lock, under a
  context of its own that its waiting callers share; the last to leave cancels it.
- **Ask 10:** `Handler`, `ServeStream` and `ServeStreamProjection` refuse a positive
  `ReauthorizationInterval` without a positive `WriteTimeout`, and the
  [revocation budget](../auth.md#revocation-budget) is documented in one place.
- **Ask 13:** the [SubjectAccessReview request](../auth.md#what-the-subjectaccessreview-asks) is
  documented on the adapter and in the guide, and tests pin it. The exported attribute builder is
  deferred.

Asks 11 and 12 are deferred; the proposal records what would reopen them.

## The asks at a glance

| # | Ask | Priority | What krm-foyer does meanwhile |
| --- | --- | --- | --- |
| 9 | Open a shared watch without holding the backend-wide lock, and cancel an opening nobody waits for | High | Its upstream backend returns at once and opens the watch on the first `Next` |
| 10 | Make timed rechecks independent of a blocked write, or require a write timeout with them | High | Sets `WriteTimeout` on every stream, and counts it in its revocation bound |
| 11 | Let a host trigger a recheck | Medium | Rechecks on the timer only |
| 12 | Recheck once per principal and scope, not once per subscriber | Medium | Reuses decisions for a short time in its own authorizer |
| 13 | Say which attributes the SubjectAccessReview asks about | Low | Keys its decisions on them, with a test that fails if they change |

## Ask 9: open a shared watch without holding the backend-wide lock

**The problem.** `SharedBackend.Watch` holds `b.mu`, the lock every scope takes, while
`startScope` calls `b.upstream.Watch`, a network round trip to the API server. The
context it passes is `context.WithCancel(context.Background())`, and nothing cancels it
until the scope exists. So one API server slow to answer one opening:

- **holds up every other scope:** a stream of an unrelated namespace waits for the lock,
  and never reaches the API server;
- **cannot be abandoned:** the browser that asked for it leaving cancels its own request,
  but not the opening, so its stream slot stays taken, and so does every other waiting
  stream's.

krm-foyer reproduced both: an API server that never answers a watch of one namespace,
not even with headers, held up a stream of another namespace until the test timed out,
and the first stream's slot stayed taken after its browser left. A client-go watch has
no response-header timeout of its own, so the wait is unbounded.

**What we suggest.** Open the upstream outside `b.mu`: record an in-flight entry for the
scope under the lock, so later subscribers of the same scope wait on it, then open
without the lock. Give the opening a context that its waiting subscribers can cancel
together: when the last one leaves before the watch opened, cancel the opening and
drop the entry. The backoff bookkeeping can stay under the lock.

**What krm-foyer does meanwhile.** Its upstream backend's `Watch` returns a watcher at
once, and opens the real watch on the first `Next`, with the context `Watch` was given.
`pump` calls `Next` without the lock, and `leave` cancels that context when the last
subscriber goes, so the opening is the scope's alone and ends with it. An opening
failure arrives through `Next` and `die`, which already records an
`UPSTREAM_UNAVAILABLE` for the backoff. This works, but it relies on the order of calls
inside `SharedBackend`, which is not a contract.

## Ask 10: keep timed rechecks from waiting on a blocked write

**The problem.** Timed checks and delivery share one gate per subscriber (docs/auth.md:
"Timed checks run per subscriber and pause that subscriber's object delivery"). A write
to a browser that stopped reading blocks once the buffers between are full, and the
recheck waits behind it. `ReauthorizationTimeout` does not cover that wait. With
`WriteTimeout` zero, the default, the write blocks until something else ends the
request, so a revoked grant is never applied to that stream. krm-foyer reproduced it: a
stream whose reader stopped, its buffers filled, its grant revoked, stayed open with no
further reviews, past a bound of about 10 seconds.

docs/auth.md does say the bound "assumes ... sinks do not block indefinitely", and the
sharedstream example sets `WriteTimeout`. But nothing ties the two together, and a host
that sets `ReauthorizationInterval` alone gets an interval that does not hold.

**What we suggest,** either of:

- **Refuse the combination:** `Handler` panics on `ReauthorizationInterval > 0` with
  `WriteTimeout == 0`, as it does for other unsafe options, or defaults `WriteTimeout`
  then; or
- **Decouple them:** run timed checks apart from delivery, and on a denial end the
  request (close the connection), whatever the write in progress is doing.

Either way, document the whole revocation budget in one place: interval, plus the check
timeout, plus one write timeout, plus cleanup, and whatever caching the host adds.

**What krm-foyer does meanwhile.** It sets `WriteTimeout` (10 seconds, configurable) on
every stream, shared or not, and its documented bound adds it in. A test stops reading,
fills the buffers, revokes the grant, and sees the stream end within the bound.

## Ask 11: let a host trigger a recheck

**The problem.** Rechecks are timed, per subscriber, and only the interval can be
tuned. A shorter interval revokes sooner and costs more reviews; a longer one the
reverse. A host that learns of a change sooner (it watches RBAC's Roles and Bindings, a
webhook authorizer's policy, or its own session store) has no way to apply it before
the next tick.

**What we suggest.** An API on the gateway to recheck now, for every subscriber or for
those matching a principal or scope, for example `Gateway.Reauthorize(func(Principal,
Scope) bool)`, through the same path as a timed check, so a denial ends only those
streams. A host could then pair an event-driven recheck (immediate for RBAC changes)
with a long periodic one as a safety net for what it cannot watch, such as group
membership from the issuer. For krm-foyer that would make the common revocation
immediate and cut the periodic review load several times over.

**What krm-foyer does meanwhile.** Rechecks on the timer; its decision guide explains
the trade-off between the interval and the load ([watches](https://github.com/ConfigButler/krm-foyer/blob/main/docs/watches.md#load-on-the-api-server)).

## Ask 12: recheck once per principal and scope

**The problem.** Each subscriber has its own recheck timer. A user with nine tabs on one
scope is nine timers, each asking the same two questions, and the timers drift apart
as the tabs open at different times. The reviews then scale with streams, not with
users: with 200 users each on nine scopes, about 120 reviews a second at a 30-second
interval, against 13 for one scope.

**What we suggest,** either of:

- one timed check per (principal, scope), whose answer applies to every subscriber of
  that pair; or
- a decision cache in `kube`, keyed on exactly the attributes a review sends, that a
  host can wrap `SubjectAccessReviewAuthorizer` with, with a lifetime the host chooses
  and counted in the revocation budget. Every shared host needs one, and getting the
  key wrong is a disclosure, so it belongs next to the authorizer.

**What krm-foyer does meanwhile.** Its own authorizer wraps the SubjectAccessReview
authorizer with such a cache: a decision is reused for 10 seconds for exactly the same
subject and asked attributes, two checks at once wait for one answer, and an error is
never kept. A fuzz test checks that the cache never changes an answer. Reuse still
depends on the timers falling within one lifetime of each other, which ask 12's first
form would make unnecessary.

## Ask 13: say which attributes the review asks about

**The problem.** A host that caches decisions has to know exactly which parts of a scope
the review asks about. Today `review` sends the group, version, resource, namespace and
name, and not the label selector (Kubernetes' `ResourceAttributes` has a `LabelSelector`
since 1.31, unused here). That is reasonable, since RBAC cannot grant by label, but it is
written down nowhere, and a later version that sends selectors would make a cache keyed
without them wrong.

**What we suggest.** Document the attributes on `SubjectAccessReviewAuthorizer`, and
mention a change to them in the changelog as a breaking change for hosts that cache. An
exported function returning the `ResourceAttributes` for a scope and verb would let a
host key on exactly what is asked.

**What krm-foyer does meanwhile.** Keys on those five attributes and the subject, and a
test fails if a review it observes carries a selector.

## What we did not ask for

- **A response-header timeout in the Kubernetes backend.** Ask 9 is the real fix; with
  it, a slow opening costs only its own scope's streams, which their browsers can
  abandon.
- **Sharing by default.** We agree with keeping `SharedBackend` opt-in: an identity that
  may read everything, and the review load, are not costs every host should carry
  unasked. krm-foyer keeps it opt-in too ([why](https://github.com/ConfigButler/krm-foyer/blob/main/docs/watches.md#why-sharing-stays-opt-in)).
