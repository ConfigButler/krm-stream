# Proposal 0006: Delivery plan for watch streams and optional editing

**Status: active delivery plan, updated 2026-10-05.** Implementation baseline: the stacked branch
through `b5cc779`. This is the single inventory of completed work, open work and ordering. Detailed
contracts stay in the linked proposals and guides rather than becoming another roadmap.

Start with [watching resources](../why-a-gateway.md); add the [editor](../client-state-model.md) only
where a page needs drafts. Native Kubernetes watches through fetch are the preferred direction;
retain gateway SSE compatibility and the projected gateway's redaction, suppression and sharing.
Follow the [design rules](../../CONTRIBUTING.md#design-rules) and [release policy](../releasing.md).
[Proposal 0005](0005-kubernetes-stream-and-save-semantics.md) owns the stream/save tradeoffs.

## Completed work

“Implemented” below means present in this checkout; it does not establish that the current stack is
merged, released or adopted by a host. Documentation changes are in the working tree. Final-commit
validation and consumer dependency upgrades remain separate delivery steps.

| Done now | Result | Reference |
|---|---|---|
| Defined views and suppression | Full/spec Secret-value redaction, status omission in spec, redaction revisions and suppression excluding RV | [Proposal 0004](0004-views-and-bytes.md) |
| Gateway snapshot and shared-watch baseline | Complete snapshot boundaries, UID membership, optional watch sharing, bounded configured HTTP delivery and lifecycle observations | [Watching](../why-a-gateway.md), [proposal 0007](0007-shared-stream-host-integration.md) |
| Shared-watch hardening | Independent/cancellable openings, write-timeout requirement with timed authorization and exact SubjectAccessReview inputs | [Proposal 0008](0008-shared-watch-hardening.md) |
| Browser connector separation and fixes | One fetch/SSE connector delivers state events; bounded recovery, terminal refusal, callback-failure cleanup, protocol diagnostics and adopted-save membership fix | [Proposal 0009](0009-stream-and-editor-separation.md#implemented-connector-and-correctness-fixes) |
| Go API cleanup | Shared `StreamConfig`, consolidated serving/constructor names and internal repository harness | [Proposal 0010](0010-gateway-api-cleanup.md), [migration](../migrating.md) |
| Stream convergence contract | Projected-content guarantee and executable final-write evidence | [Conformance](../../conformance/README.md#convergence-evidence) |
| Optional editor and safe-save baseline | Draft reconciliation, explicit conflicts, atomic intent capture and guarded projected reads; recovery remains review plus another deliberate Save | [Editor model](../client-state-model.md), [saving](../saving.md) |
| Editor integration and recovery recipes | Tested deletion recovery, keep-local resolution and Vue subscription ownership | [Recipes](../../examples/editor-recipes/README.md), [Vue](../../examples/vue/README.md) |
| Real-API save composition | Cases for suppressed churn, guarded-read overlap, structured errors and same-name UID replacement | [Completed baseline](#completed-baseline) |
| Watch-first documentation | README, watch/edit/save guides, native-fetch request and compact decision records; duplicate/superseded guides removed | [README](../../README.md), [current request](../field-reports/third-our-identity.md) |

## Open work and delivery order

The numbers express the recommended order of attention, not a requirement to finish every earlier row
before starting a later one. Deliver separately reviewable changes and use the prerequisites below.

| Order | Open work | Status / prerequisite | Completion result |
|---|---|---|---|
| 1 | Settle the native watch contract and comparison workload | Requested; start now using the existing event-based connector seam | Reviewed native view/edit contract, initial-list/resume rules, stale/live presentation, auth/expiry handling and SSE compatibility; one declared workload |
| 2 | Finish the editor API cleanup in proposal 0009 | Proposed; can proceed alongside native design; coordinate shared state-input changes | Bound event input, removal of unguarded/duplicate APIs, simpler redaction reads, trimmed exports, migrated examples and upgrade guide |
| 3 | Implement the native fetch connector with recovery acceptance | After native contract review; transport can use today's event consumer without waiting for editor cleanup | Native JSON directly becomes state events; paginated-list fallback, safe resume/resnapshot, terminal auth refusal, bounded recovery and disposal pass acceptance |
| 4 | Measure and implement gateway upstream continuation | Design exists below; independent of native connector and editor cleanup | Reviewed reopen/time bounds, same-workload baseline, tests and measured continuity without routine downstream resets; safe fallback and authorization preserved |
| 5 | Evaluate save progress during suppressed churn | Requested evaluation; existing real-API fixtures suffice; independent of native transport | Measured decision on bounded submitted-intent recovery first, then optional coalesced version delivery; baseline retained if benefit is insufficient |
| 6 | Deliver the comparative frontend and measurements | Baseline gateway/shared-watch measurements can start now; native comparison follows order 3; editing uses the API selected in order 2 | One viewer plus optional editor compares native/full/spec and shared/unshared runs, including conflicts, expiry, reconnect and synthetic Secret disclosure checks |
| 7 | Validate, release and adopt each ready change | Per change, after its own acceptance and final-commit CI; no release requires all tracks to finish | Matching npm/Go artifacts, migration guidance and host dependency updates with browser/session/RBAC acceptance |

Native design, implementation and comparison are specified in the
[current request](../field-reports/third-our-identity.md#native-watch-connector). Editor cleanup is
specified in [proposal 0009](0009-stream-and-editor-separation.md#proposed-editor-cleanup). Gateway
continuation and save-progress evaluation retain their detailed sections below.

### Dependencies and next steps

1. Review the native source/view and recovery contract, including the editor's machinery-field and
   host-validation rules. Agree the test workload before adding another public connector.
2. In parallel work tracks, review and finish the pending editor cleanup, review the existing gateway
   continuation design and record gateway/save baselines. This describes scheduling; each change has
   its own review and acceptance.
3. Implement native transport after its contract is settled. The current event consumer is enough for
   transport work; a dedicated read-only store and new package entry points are not prerequisites.
   Finalize the supported editor input before publishing the native editing example or new editor APIs.
4. Run the comparison against whichever implementations are ready, naming each tested revision and
   feature set. Do not attribute native resume savings to gateway continuation or sharing savings to
   suppression. Save-policy implementation requires a favorable evaluation and reviewed follow-up scope.
5. Validate and release ready increments independently under the lockstep policy. Check consumer pins
   and host acceptance before marking adoption complete. The existing implemented stack can be
   validated/released without waiting for the new native feature.

There are no assigned dates or owners in this plan. Open design decisions are native view identity and
editable machinery, stale/live transitions on resume, gateway reopen deadlines/credential lifetime,
and the save-recovery policy and version-delivery compatibility decision. Resolve them in their
respective contract reviews; none authorizes a silent change to current projected-view semantics.

## Deferred work

| Item | Trigger for reconsideration | Current approach |
|---|---|---|
| Dedicated `ResourceStore`, shared snapshot-tracker extraction and stream/editor subpaths | Demonstrated viewer or unbundled-loading need with measurable benefit | `LiveResourceStore(readOnlyPolicy)` and the root/combined bundle; no extra store is required for native transport |
| Authorization triggers, grouped checks, decision cache and exported review helpers | Measured duplicate authorization work or a host lifecycle unmet by cancellation/timed checks | Existing per-subscriber checks and host-owned policy; [proposal 0008](0008-shared-watch-hardening.md#deferred-work-and-conditions-for-reconsideration) |
| Version-delivery or automatic save-recovery implementation | Order 5 establishes benefit, safe scope and explicit compatibility | Current suppression and another deliberate Save; evaluating alternatives is scheduled, implementation is conditional |
| Browser replay for gateway SSE, independent delivery switches, write tickets and general SSA abstraction | Separate concrete use case and reviewed protocol/write contract | Fresh browser snapshots, named projections and host-owned conditional merge PATCH |

## Completed baseline

Convergence is defined by [the protocol and executable evidence](../../conformance/README.md#convergence-evidence).
The [editor recipes](../../examples/editor-recipes/README.md) cover deletion recovery and explicit
keep-local resolution through store events. Preserve their draft, UID, policy and disposal guarantees
when changing the editor API.

The `TestRealAPI` composition cases in [composition_e2e_test.go](../../gateway/kube/composition_e2e_test.go)
exercise status-subresource churn, bookkeeping-only suppression, overlapping guarded reads and
same-name UID replacement. They use the real store and conditional editor under node, a Widget CRD
and a test-only preflight/PATCH race barrier. They establish 409 rejection, guarded recovery and a
fresh deliberate save after churn stops; they do not establish usable save progress during sustained
churn. `task test-real-api` runs these cases. Record actual runs, server versions, skips and final
commit separately from fake-client and general CI results.

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
  `Clients` refresh calls. Timed subscriber checks must keep running during upstream recovery.
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
roughly 250 ms, 1 s and 4 s. These delays total about six seconds; opening/network time is additional
and each reopen needs an explicit cancellable time bound before claiming a total recovery limit. A reopened
watch that closes again before delivering any event, bookmarks included, counts as a failure, so a
server that accepts and immediately drops watches cannot loop. The count resets when a reopened
watch delivers an event. At the bound the watcher returns `ErrWatchClosed`: the gateway starts a
fresh cycle, its existing rule turns a second early end into `UPSTREAM_UNAVAILABLE` and closes the
connection, and the client's own bounded retry budget takes over. Measure time to downstream
recovery/closure including opening delays; the backoff total alone does not bound stale `live`
presentation. No public retry setting is proposed without a demonstrated need.

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

## 5. Save progress under suppressed churn

**Status: requested evaluation, not an implemented retry or delivery feature.** A spec editor should
avoid repeated extra Save clicks when controller changes leave its relevant editable values unchanged.
The adopter has not yet measured frequency in a representative workload. Reuse the real-API fixtures
above and [proposal 0005's tradeoffs](0005-kubernetes-stream-and-save-semantics.md#host-write-strategies).

Compare today's guarded read plus another deliberate Save with two optional approaches. Evaluate
bounded save recovery first; periodic version delivery still races changes between delivery and PATCH.

### Bounded recovery of a submitted intent

After a definite version rejection, refresh and reconcile under the existing UID, revision and
snapshot guards. Determine whether the original submitted intent is still valid by comparing its
relevant base values and dependencies. No field conflicts in the store is insufficient by itself.
A retry must preserve the submitted patch, exclude later typing, protect whole-array replacements
and refuse changed dependencies or replacement UIDs. Each candidate retry captures the validated
original intent and its new precondition together; never just replace the RV on an old patch.

Keep explicit review for real disagreements. Bound attempts and elapsed recovery, explain exhaustion
and preserve drafts. Distinguish a definite 409 rejection from an unknown write outcome; do not
replay an ambiguous write automatically. Retain host authorization, projection validation and
serialization of writes. Any public recovery policy requires demonstrated use and its own contract.

### Optional coalesced version delivery

Retain the latest eligible version per subscriber and UID and compare periodic batches of small
version-only records with batched complete projected updates. Proposal 0005 currently prefers
existing event shapes if measurements justify more version delivery; do not choose a wire extension
in advance.

Eligibility requires proof that the last delivered authoritative projected content, excluding RV but
including redaction records, still represents that UID at the newer version. The draft is not the
baseline. Bind delivery to the current view and snapshot and apply it in order. A pending record may
not overtake a visible update, resurrect a delete, roll a version backwards or cross a reconnect.
Clear or supersede pending records on those transitions. Collection bookmarks cannot supply an
object's edit version. Hidden Secret changes require normal redaction updates.

Advance authoritative versions without resetting drafts or waking content-only subscribers. Capture
future save versions and patches together; do not mutate already captured intents when a batch
arrives. Declare which writes can rely on the guarantee, including dependencies outside the view.
Periodic delivery can reduce staleness but cannot eliminate the delivery/PATCH race.

### Evidence and decision

Use identical objects, subscriber counts, edits and status rates for the baseline and variants.
Include quiet periods, bursts and sustained churn. Measure stale-version 409s separately from field
conflicts, extra clicks, save completion latency, successful saves and bounded failures during churn,
GET/PATCH attempts, downstream bytes/events, notifications/renders and batch memory/delivery work.
Success after churn stops is insufficient evidence of usable editing during churn.

Cover later typing, changed array members, concurrent spec edits, deletion/recreation, partial
snapshots, resets/reconnects with pending batches, expired sessions, denied access and hidden Secret
rotations. Preserve current `krm-spec/v1` status-only silence by default. Any new delivery mode needs
an explicit opt-in and compatibility decision; do not silently wake current consumers.

Document measurements, the decision and adoption guidance. Retaining the baseline is a valid outcome
if added costs outweigh measured benefit. A universal automatic-save policy is outside this request.

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

Order 5 evaluates submitted-intent recovery and coalesced version delivery without authorizing
implementation or changing default emissions. Independent content/delivery switches, downstream
replay, write tickets and a general SSA abstraction remain deferred. Preserve current named-view
semantics, host policy and the distinction between native browser and gateway upstream recovery.
