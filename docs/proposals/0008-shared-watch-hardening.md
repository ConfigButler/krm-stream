# Proposal 0008 Shared watch hardening implementation plan

**Status: historical implementation plan; selected scope implemented, asks 11 and 12 deferred.**
It is kept as the record of what was decided and why; its instructions were for the implementer and
are not adopter guidance. For current guidance see
[session validity and timed checks](../auth.md#session-validity-and-timed-checks), the
[revocation budget](../auth.md#revocation-budget),
[what the SubjectAccessReview asks](../auth.md#what-the-subjectaccessreview-asks) and
[shared backends](../../gateway/README.md#shared-backends).

This is the implementer handoff for the [second krm-foyer field report](../field-reports/second-fieldreport.md).
Implement ask 9, the explicit HTTP configuration validation option from ask 10, and the
documentation portion of ask 13. These changes repair shared-watch opening and make the
requirements for timed authorization clearer. Asks 11 and 12 are deferred; they are not
acceptance criteria for this work.

The local source review used commit `869ea81`. The field report describes a different baseline,
`5ca19ed`, and external krm-foyer reproductions. Those reproductions have not been run in this
repository. Confirm the implementation baseline and add repository-owned tests before changing code.

## Decisions and work order

| Order | Deliver | Acceptance evidence |
| --- | --- | --- |
| 1 | Ask 9: independent, cancellable shared-watch opening | Deterministic concurrency tests and Go race checks |
| 2 | Ask 10: require a positive HTTP write timeout with timed reauthorization | Construction and direct-serving validation, plus blocked-reader HTTP coverage |
| 3 | Ask 13: document the actual SubjectAccessReview request | Complete request assertions and matching API and adoption documentation |

Deliver these as separate reviewable changes. They do not depend on authorization caching,
event-driven policy integration, new wire events, or browser changes. Keep the existing save and
draft-recovery work in [proposal 0006](0006-stream-and-save-implementation-plan.md) independently
deliverable. Follow [design rules](../../CONTRIBUTING.md#design-rules) and
[release policy](../releasing.md).

## 1 Shared watch opening

### Required behaviour

In [shared.go](../../gateway/shared.go), move upstream `Watch` calls outside the backend-wide
mutex. Waiters must be able to abandon an opening through their own contexts. The upstream
opening must have a scope-owned context: cancelling the first caller must not cancel an opening
that another caller still needs.

Maintain one current opening per scope. Register an opening under the lock, perform network work
without that lock, and publish its result only if that entry is still current. Count waiting callers
so the final departure cancels and removes an unfinished opening. Waiters for unrelated scopes must
continue independently. Keep the current live subscription lifetime: once attached, `Stop` releases
the subscription and the last subscriber cancels the shared upstream.

Handle the handoff between opening and subscription explicitly. There must be no gap in which the
opening has no waiters or subscribers but retains an upstream watch. If an abandoned upstream
returns a watcher late, stop it and discard it. Its result must not delete a replacement opening,
populate its cache, or change its backoff.

The implementation may use a private opening entry with a completion signal and waiter accounting;
no public opening API is required. Preserve existing snapshot framing, cache ownership, queue
overflow recovery, failure classification, backoff and lifecycle observations. A cancellation caused
only by all waiters departing is not an upstream failure and must not impose outage backoff.

### Acceptance tests

Use channel barriers and a fake upstream that can block inside `Watch`; avoid sleep-based ordering.
Tests must cover:

- Scope A blocks while scope B opens and delivers its snapshot.
- Concurrent callers for the same scope cause exactly one current upstream opening.
- Cancelling one waiter returns promptly without disrupting remaining waiters.
- Cancelling the final waiter cancels the upstream context, removes the opening and permits a retry.
- A caller whose context is already cancelled does not create or retain an upstream opening.
- Cancellation racing with successful opening leaves neither a watcher nor a subscription orphaned.
- An abandoned opening returning late cannot mutate a newer entry; any returned watcher is stopped.
- A genuine retryable opening failure updates backoff once, and all relevant waiters receive the
  classified failure. Existing terminal failures and retry hints retain their behaviour.
- Warm-cache joins, last-subscriber cleanup, overflow and balanced observations remain correct.

Cancellation guarantees assume the upstream honours context cancellation. Do not promise to forcibly
terminate an arbitrary host backend; ensure its late result cannot affect current state.

## 2 HTTP configuration for timed reauthorization

### Chosen implementation

Require `WriteTimeout > 0` whenever `ReauthorizationInterval > 0` for library-owned HTTP serving.
Reject the invalid combination with a clear configuration panic, consistent with existing option
validation. Do not silently select a timeout.

Validate at `Handler` construction and before any response I/O in direct `ServeStream` and
`ServeStreamProjection` calls. Share private validation where useful so these paths cannot diverge.
Retain existing rejection of negative write timeouts. A zero write timeout remains valid for HTTP
streams with timed reauthorization disabled.

Transport-neutral `Stream` and `StreamProjection` must continue to support timed checks with
host-owned bounded sinks. Do not require an HTTP-only option for those methods. Update the
`Options` and `Gateway` comments to distinguish these paths.

Preserve the delivery gate and existing per-subscriber authorization and projection checks. This
increment does not introduce a cancellable gate, independent authorization scheduler, or force-close
API. Positive timeout capability checks must retain their current fail-before-streaming behaviour.

### Documentation and limits

Update [auth.md](../auth.md), [operations.md](../operations.md), [adopting.md](../adopting.md), and
affected examples. The timed-recheck example must set both authorization and write budgets.

Explain that `ReauthorizationTimeout` starts after gate acquisition and covers the periodic callbacks.
Describe the revocation budget in one place, including timer scheduling, gate waiting, callback
timeout, host cache freshness, and termination/cleanup. Link to it from the other guides.
Do not turn interval + callback timeout + one write timeout into an unconditional bound:
terminal delivery, competing I/O, scheduling and host cleanup can add time. Distinguish cessation of
new object delivery, request return, and browser receipt. Already buffered bytes cannot be recalled.
Opening and cycle authorization still require host callback deadlines.

### Acceptance tests

- `Handler` rejects timed reauthorization with zero write timeout at construction.
- Both direct HTTP entry points reject that combination before headers, observations for logical
  stream opening, or backend calls. Use a writer spy to prove no response I/O occurred.
- Positive write timeout with timed checks is accepted; zero timeout without timed checks remains
  accepted. Generic bounded sinks can use timed checks without an HTTP write timeout.
- Reuse or extend the real HTTP blocked-reader tests in [sse_test.go](../../gateway/sse_test.go).
  Fill transport buffers, revoke access, and verify that the request terminates within a declared
  test budget and releases its subscription. A healthy subscriber must remain unaffected.
- Preserve HTTP/1.1 and TLS HTTP/2 coverage, unsupported-writer rejection, quiet-stream survival,
  and authorization/projection denial tests. Do not require a terminal frame to reach a stalled client.

Record which coverage is reused and which scenario is new. State the test's callback, write and
scheduling assumptions; a generous test tolerance is not a published production guarantee.

This is a breaking configuration change. Include a `BREAKING CHANGE:` footer explaining that HTTP
hosts using timed reauthorization must choose a positive per-operation `WriteTimeout` and verify
their mounted middleware supports deadlines and flushing. Update repository callers together.

## 3 SubjectAccessReview documentation

Document [SubjectAccessReviewAuthorizer](../../gateway/kube/authz.go) as currently implemented:

- Subject inputs are username, groups, UID and extras, supplied by the host's `SubjectFor`.
- The adapter checks `list` and `watch`, stopping on the first refusal or failure.
- Resource attributes are verb, group, version, resource, namespace and name.
- Label and field selectors and subresource are not supplied. Target and projection are not SAR
  resource attributes; the configured client selects the authority receiving the review.

Add this contract to the exported API comment and shared-watch authorization guide. State that
selectors currently do not narrow the authorization question. Keep this description specific to the
adapter; do not generalize it into a guarantee about all Kubernetes authorization policies.

Extend [authz_test.go](../../gateway/kube/authz_test.go) to assert both verbs, every transmitted
subject/resource attribute, and the omitted selector/subresource fields. Include a named scope and
a scope carrying selectors so omissions are deliberate and tested.

Explain that a host cache must distinguish the complete subject and request, including verb, and
isolate decisions by authority or target binding. Reusing a SAR result must not bypass current host
session validity or projection checks. No cache implementation or exported attribute builder is
part of this increment.

Future changes to the documented authorization inputs require compatibility review and explicit
release notes for caching hosts. The current documentation change does not itself change SAR
semantics. Do not manually edit generated changelogs; use conventional commits and PR details.

## Deferred work and conditions for reconsideration

**Ask 11:** no gateway-wide `Reauthorize(predicate)` API in this plan. Reconsider a minimal trigger
only with a concrete host integration, stream lifecycle and trigger-coalescing rules, a distinction
between scheduling and completion, and coordination with any authorization cache invalidation.
Policy watches and determining affected subjects remain host-owned. Preserve periodic checks as a
fallback, and do not promise immediate revocation from a trigger alone.

**Ask 12:** no grouping scheduler and no decision cache in this plan. `Principal` is opaque; matching
users does not establish interchangeable sessions or projection policy. Correct the workload before
evaluating benefit: 200 users with nine tabs on one scope produce about 120 periodic SARs/s at a
30-second interval and could reduce to about 13/s with deduplication. Nine distinct scopes per user
still require about 120/s when grouping by principal and scope. These counts assume both verbs are
allowed and exclude opening and recovery checks.

An optional kube cache can be reconsidered with measured duplicate requests, a complete key,
authority isolation, bounded memory, explicit TTL and invalidation, error handling and waiter
cancellation semantics. Deduplicating SARs must remain separate from subscriber-specific host
authorization and projection decisions. An exported attribute builder also remains deferred.

## Verification and completion

For each implementation PR, add focused tests proving the changed behaviour. Before opening the PR,
run `task fixtures-check`, `task test`, and `task lint` as required by CONTRIBUTING. Run the affected
Go package tests with the race detector for shared opening changes. Inspect existing test/task setup
to run each Go module correctly. No new wire fixture is needed unless implementation unexpectedly
changes the shared protocol contract; such a change requires separate scope review.

The final handoff from the implementer must identify the tested commit, commands and results,
unrun external scenarios, compatibility change, and remaining limitations. All three selected
increments must be complete; deferred asks must not be reported as implemented. No benchmark,
external krm-foyer reproduction, universal capacity claim, or new real-cluster campaign is a merge
requirement for this bounded work.

## Draft response to the krm foyer team

Thanks for the concrete failure cases and the explanation of your workarounds. We are taking on
ask 9, the explicit HTTP validation option in ask 10, and documentation of the current SAR inputs
from ask 13.

We will isolate upstream openings by scope and cancel an unfinished opening when its final waiter
leaves. For HTTP streams with timed authorization, we will require an explicit positive write timeout
and document the budget and its limits. We will document the exact review inputs and add tests
protecting them.

We are deferring the public trigger API, grouped authorization scheduler, cache and exported
attribute builder. These need additional contracts around session identity, policy checks,
invalidation and measured duplicate load. Please distinguish nine tabs on one scope from nine
distinct scopes in the load example; grouping only removes duplicate questions for the same pair.

Please share the opening-cancellation and blocked-reader reproductions with their tested commits and
transport settings. They will help us compare repository tests with your integration. Also include
verb and authority isolation explicitly in the cache-key description. We will provide a configuration
migration note with the timed-authorization change. This scope does not promise immediate revocation
or a universal bound for arbitrary host callbacks and sinks.
