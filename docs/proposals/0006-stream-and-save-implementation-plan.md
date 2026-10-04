# Proposal 0006: Remaining stream and save work

**Status: active follow-up plan.**

Follow the standing [design rules](../../CONTRIBUTING.md#design-rules) and
[release policy](../releasing.md). [Proposal 0005](0005-kubernetes-stream-and-save-semantics.md)
explains the unresolved tradeoffs; this document owns work order and acceptance criteria.

[Proposal 0007](0007-shared-stream-host-integration.md) proposes bounded HTTP delivery, lifecycle
observations and tested shared-host composition. That work can support the authorization bounds
and continuation measurements below, but adds no dependency or acceptance gate to this plan.
Hosts may demonstrate the existing requirements with their own bounded sinks and instrumentation.

[Proposal 0009](0009-stream-and-editor-separation.md) separates browser stream consumption from the
optional editor layer. It owns that API and package-boundary work; this plan retains the existing
behavioral follow-ups and acceptance criteria. Priorities 2–3 below form the editor track, and
priority 4 forms the stream track. Deliver them independently. Complete the browser separation
before expanding the editor's public API; it does not block gateway fixes, real-API evidence or
upstream continuation.

[Proposal 0010](0010-gateway-api-cleanup.md) owns separate Go API cleanup. Use its final serving and
configuration names once implemented; it adds no acceptance gate to the behavioral work here.

## Baseline and order

Use the [adoption guide](../adopting.md), [saving guide](../saving.md) and
[conditional editor](../../examples/conditional-save/README.md) for current behavior.

| Priority | Remaining work | Completion evidence |
|---|---|---|
| 1 | Define convergence precisely — complete | [Evidence](../../conformance/README.md#convergence-evidence) |
| 2 | Publish tested deletion-recovery and keep-local recipes — complete | [Recipes](../../examples/editor-recipes/README.md) and [tests](../../packages/krm-stream/test/recipes.test.ts) |
| 3 | Harden real-API save composition and identity races — implemented | [`TestRealAPI` cases](../../gateway/kube/composition_e2e_test.go), run by `task test-real-api` |
| 4 | Measure and implement upstream continuation | Same-workload comparison proves continuity, bounded recovery and authorization. |

Review the remaining priorities as separate changes. Baseline measurement can run alongside
priorities 2–3. Unit tests do not establish real-cluster composition, consumer readiness or capacity.

## 1. Define convergence precisely

Completed: [contract and executable final-write evidence](../../conformance/README.md#convergence-evidence).

## 2. Tested adoption recipes

Completed: the [editor recipes](../../examples/editor-recipes/README.md) keep a recovery copy before
deletion or pruning and keep a local value in a conflict, executed by the client suite through
stream events and linked from the saving and Vue guides. The acceptance criteria below are what
those tests cover.

Keep the existing [user-facing outcomes](../saving.md#what-the-person-editing-sees) and host-owned
[receipt contract](../saving.md#answer-204-or-a-receipt-and-let-the-watch-echo-it) in the saving guide.

For proposal 0009's create/delete guidance, use its event-only store input, consolidated connector and
guarded response contract. Successful creates/deletes remain host-owned pending confirmations until
their echo or a completed snapshot confirms state; do not restore unguarded adoption or optimistic
store deletion in a recipe. Keep server acceptance separate from confirmation to avoid duplicate
submissions when an echo is delayed or arrives before the response.

### Deletion recovery copy

Demonstrate a host subscription that snapshots detached `store.draft(uid)` on each notification while
that fixed UID exists, including the initial state. When removal or snapshot pruning makes it absent,
retain the last copy for explicit copy-out; do not try to read the removed draft. Capturing only on
Save misses later typing. Specify subscription cleanup, identity-scoped retention and expiry.

**Acceptance:** edits immediately before deletion and snapshot pruning remain recoverable; a
replacement UID opens separately and never inherits the old draft. Test initial capture, edit-time
updates, null-state handling and disposal in the actual recipe. Keep one active draft store; the
recovery copy is not another reconciler. Link the tested recipe from the saving and Vue guides.

### Explicit keep-local resolution

Demonstrate capturing the chosen local value (including absence), resolving that path with
`revert`, then synchronously reapplying it through `setValue` or `removeKey`. Proposal 0009 removes
the equivalent `takeTheirs` alias. Verify the recipe against current behavior before publishing
executable guidance.

**Acceptance:** cover nested paths, deletion, whole-array replacement and policy/redaction refusal;
preserve unrelated edits and conflicts. Capture a fresh save intent after review. Add a small helper
only if repeated consumer code warrants it; do not create another conflict registry.

## 3. Real-API composition and identity races

Implemented as the `TestRealAPI` cases, run by `task test-real-api`. The browser side runs the real store and conditional editor under node. A
real status subresource comes from a test-installed Widget CRD, and a test-only transport barrier
recreates the ConfigMap between the host's preflight GET and its PATCH. The race showed that
Kubernetes checks the captured `resourceVersion` before the UID, so a replacement arrives as a plain
409. The host now classifies it with one GET and answers with structured Kubernetes `Status`
responses. The criteria below are what those cases assert.

The [existing real-API stale-RV test](../../gateway/kube/e2e_test.go) proves ordinary rejection, not
these compositions. Extend it alongside [store/example tests](../../packages/krm-stream/test/saving.test.ts)
and [host handler tests](../../gateway/kube/examples/conditionalsave/handler_test.go).

- **Status churn:** use a real status subresource. Prove a persisted status change advances RV,
  spec projection suppresses its notification, stale PATCH returns 409, guarded projected GET
  advances the base without field conflicts, and a fresh intent succeeds when churn stops. Contrast
  full projection and bookkeeping-only suppression. A no-op SSA reapply is not a valid fixture.
- **Overlapping recovery:** save starts live, status advances RV, upstream closure begins a snapshot,
  and the post-409 GET arrives before synced. Assert refused reconciliation, retained drafts,
  recovery presentation and later progress without blind PATCH retries.
- **UID race:** force deletion/recreation between host preflight GET and PATCH with a test-only
  barrier. Preserve structured API Status and prove the replacement is unchanged. Classify the
  observed identity mismatch specifically; never normalize all 422 validation errors to conflicts.

**Acceptance:** observable winner/draft preservation and accurate outcomes, with the exact tested
commit, server version, commands, scenarios and results attached to the PR. Use existing host
validation boundaries; do not generalize the ConfigMap endpoint just to build a test.

Wire focused real-API cases into CI or an explicitly invoked workflow whose successful run is merge
evidence. Keep broader aggregated-API coverage available through `task test-cluster`. Record skips
and unrun cases separately from fake-client results; update task/workflow comments to match coverage.

## 4. Measured upstream continuation

Implement behind [the Kubernetes backend](../../gateway/kube/backend.go) and its existing `Watcher`
seam. Assess the pinned client-go utilities first; explain why one fits or why a small internal loop
is needed. Downstream v1 remains snapshot-based on a new connection.

- Retain checkpoints from upstream events and bookmarks before projection/suppression. Never derive
  them from a browser object, SSE seq or shared projected view. Do not require bookmark cadence.
- Continue only with a trustworthy checkpoint and established initial boundary. A failed partial
  snapshot must not be relabeled as live or completed by guessing.
- Preserve both streaming-list and list-then-watch paths, including aggregated APIs that reject
  streaming lists. The [recorded cluster observations](../facts/observed-v1.36.2+k3s1.md) are evidence
  for that environment, not universal claims about every Kubernetes API server.
- Reuse existing error and cancellation seams. Bound backoff and repeated failure handling so a dead
  upstream cannot leave subscribers reporting live forever. Specify the exhaustion transition before
  implementation; avoid adding public retry knobs without a demonstrated host need.
- Recheck the auth implications: fewer cycles mean fewer cycle-only authorization checks and fewer
  `ClientFor` refresh calls. Timed subscriber checks must keep running during upstream recovery.
  Document refreshing credentials and revocation expectations; do not accidentally weaken them.
- Keep single-subscriber overflow recovery and the shared cache's ownership intact. Last subscriber
  departure must cancel an in-progress reconnect. No second shared-watch implementation.

Test retained-history continuation with at least two subscribers: a routine close causes one upstream
reopen, no new downstream reset, and subsequent changes reach both. Test checkpoint progress on a
suppressed update/bookmark, deletion during the interruption, 410 fallback, partial initial snapshot,
forbidden response, repeated transient failure, cancellation and subscriber departure. Exercise both
initialization paths against the real API server where supported.

Measure baseline and changed runs with the same workload: upstream reopen count, downstream reset
count by cause, snapshot bytes and duration, browser deserialization/reconciliation cost, recovery
latency, access-review rate/latency and save 409 rate. Also record the fraction of 409s with no field
conflicts and whether the next deliberately captured save succeeds once churn stops. Include routine
recycling, browser reconnects, slow subscribers and access revocation. Keep metric labels bounded;
do not label by UID, username or opaque RV. One shared watch still incurs a snapshot transfer and
browser reconciliation per subscriber; measure those costs rather than inferring capacity from
upstream watch count.

### Proposed design (for review before implementation)

Nothing below is implemented. It answers the questions this section requires settled first: the
client-go assessment, the checkpoint and boundary rules, the exhaustion transition and the
authorization consequences.

**Where.** In `gateway/kube`, inside the watcher the backend returns. The gateway core, its snapshot
loop and `SharedBackend` do not change: to them a continued watch is one upstream watch that never
ended. One reopen therefore serves every subscriber of a shared scope, and nothing new reaches the
browser: no event, no reset, no replay protocol.

**Why not client-go's `RetryWatcher` (v0.36.0).** It has the reopen mechanics but conflicts with
three requirements here. It needs a concrete initial resourceVersion, so it cannot own the streaming
list's initial snapshot and could only take over after the boundary. It retries every failure except
410 and authorization indefinitely, "leaving it up to the user to timeout", where this plan requires
bounded failure. And it turns 401/403 into terminal error events, where this design wants a fresh
cycle so the host's `Clients` can supply refreshed credentials. Wrapping it to impose those rules
would be larger than a small loop in `backend.go` that reuses `translate`, `classify` and the
existing bookmark handling.

**Checkpoint.** The resourceVersion of the last event the API server delivered on this watch: added,
modified, deleted, a routine bookmark or the initial-events-end bookmark. It is recorded in the
backend, before the gateway projects or suppresses anything, so a suppressed update still advances
it. It is never derived from a browser object, an SSE `seq` or a projected or shared view. Bookmarks
only move it forward sooner; no bookmark cadence is assumed.

**Boundary.** Continuation is armed only once the snapshot boundary is established: the
initial-events-end bookmark on the streaming-list path, the list's resourceVersion on
list-then-watch. A close before that returns `ErrWatchClosed` as today, so a partial snapshot is
never relabeled as live or complete.

**Reopen.** When an armed watch closes cleanly (the API server's routine timeout, 30 to 60 minutes
by default), the watcher reopens `Watch` with the scope's selectors, `allowWatchBookmarks` and
`resourceVersion` set to the checkpoint, and without `sendInitialEvents`. Both initialization paths
reopen the same way. The reopened watch's events continue the stream, and the gateway never sees the
seam.

| Reopen outcome | Result |
|---|---|
| 410 Gone or `Expired`, when opening or as a watch error event | `ResyncRequired`: history is gone, so a fresh snapshot follows, as today |
| 401 or 403 | `ErrWatchClosed`: a fresh cycle reauthorizes and asks `Clients` again, so expired credentials are replaced; a refusal there is terminal, exactly as at opening today |
| Network failure, 429, 5xx or timeout | retry within the bound below |
| Context cancelled, or `Stop` | return at once, abandoning a reopen in progress |

**Exhaustion transition.** At most three consecutive failed reopens, with jittered backoff of
roughly 250 ms, 1 s and 4 s, so a dead upstream is noticed within about six seconds. A reopened
watch that closes again before delivering any event, bookmarks included, counts as a failure, so a
server that accepts and immediately drops watches cannot loop. The count resets when a reopened
watch delivers an event. At the bound the watcher returns `ErrWatchClosed`: the gateway starts a
fresh cycle, its existing rule turns a second early end into `UPSTREAM_UNAVAILABLE` and closes the
connection, and the client's own bounded retry budget takes over. Subscribers never report `live`
over a dead upstream for longer than that, and no public retry setting is added.

**Authorization and credentials.** Each routine upstream close used to start a cycle, and with it a
cycle authorization check and a `Clients` call. With continuation those happen far less often. That
cadence was never a revocation bound: quiet streams already relied on `ReauthorizationInterval`,
whose timed checks run per subscriber and keep running through an upstream reopen, because the cycle
they belong to has not ended. What does change is credential lifetime: a per-user backend is now
used for the whole stream rather than for one routine watch period. The 401 fallback above refreshes
credentials instead of ending the stream, and the [authorization guide](../auth.md) must say that a
backend's credentials should refresh themselves (client-go token sources do) or be renewed through a
fresh cycle. Open question for review: whether to also cap continuation age, so that a fresh cycle
still happens periodically. The proposal is not to, because timed checks and the 401 fallback cover
the cases a cap would; measurements may say otherwise.

**Shared watches.** `SharedBackend` wraps the upstream backend, so continuation happens beneath its
cache: one reopen per scope and no resnapshot for any subscriber, which also removes a
SubjectAccessReview per subscriber per routine close. Its single-subscriber overflow recovery and
early-end backoff are unchanged and still apply to the fallbacks. The last subscriber leaving stops
the upstream watcher, which cancels a reopen in progress.

**Evidence plan.** Unit tests with a fake dynamic client cover each row above, the checkpoint on a
suppressed update and on a routine bookmark, deletion during the interruption, a close during the
initial snapshot, exhaustion, cancellation and subscriber departure during backoff, and two shared
subscribers seeing one reopen and no reset. Against the real API server, a package-internal test
sets a short watch `timeoutSeconds` so routine closes happen in seconds, on both the streaming-list
path and the aggregated API's list-then-watch path. Measurements use the same workload before and
after on the spike cluster, with counts from the gateway's Observer and a counting backend: upstream
reopens, downstream resets by cause, snapshot bytes and duration, the browser store's time to apply
a snapshot (the node driver times it), recovery latency, SubjectAccessReview rate and latency, and
the save 409 rate with the share of 409s that carry no field conflicts. Labels stay bounded: no UID,
user or resourceVersion.

**Acceptance:** retained-history recycling preserves downstream continuity, lost history still
recovers safely, retries stop correctly, and authorization/credential lifecycle expectations remain
explicit. No new SSE events or downstream replay protocol.

## Verification and scope

Use [existing verification tasks](../../CONTRIBUTING.md#test-levels) appropriate to each change,
including package and browser/wire checks for runtime work. Inspect CI on the final pushed commit;
previous green commits and adopter reports are supporting history, not current validation.
Documentation-only edits need link and diagram checks, not a cluster rebuild.

Consumer acceptance remains separate: pin npm and both Go modules, check consumer CI/image
toolchains, and exercise concurrent editing, later typing during saves, recovery, session expiry and
UID replacement in the browser. Before lifecycle testing, record the host's reauthorization interval,
check timeout, maximum revocation-to-stream-closure time and concurrent subscriber workload.
Use a reference acceptance profile of 30-second rechecks, a 5-second check timeout and closure within
60 seconds at 200 subscribers. These are measurement targets, not library defaults or guarantees;
hosts choosing another profile must declare their limits before testing.

Under the declared workload, revoke access or expire a session just after a successful check, and
separately stall an authorization callback until its context expires. Pass only if every affected
stream terminates within the declared closure limit, measured from revocation/session expiry or
the start of the stalled check, respectively. Verify callbacks honor the configured check deadline
and sinks have bounded completion times. Exercise quiet and active streams; cycle-only checks cannot
meet a bounded quiet-stream revocation target. See [authorization lifecycle](../auth.md#session-validity-and-timed-checks)
for configuration and host responsibilities.

Version-only events, independent content/delivery switches, downstream replay, write tickets,
automatic conflict-free retry and a general SSA abstraction remain deferred until a concrete use
case and measurements justify them. Preserve current named-projection semantics and host policy.
