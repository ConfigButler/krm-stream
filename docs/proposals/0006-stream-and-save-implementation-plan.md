# Proposal 0006: Remaining stream and save work

**Status: active follow-up plan, reviewed 2026-09-11 against 0.3.0 and adopter feedback.**

Follow the standing [design rules](../../CONTRIBUTING.md#design-rules) and
[release policy](../releasing.md). [Proposal 0005](0005-kubernetes-stream-and-save-semantics.md)
explains the unresolved tradeoffs; this document owns work order and acceptance criteria.

## Baseline and order

Managed recovery, bounded reauthorization, differentiated save outcomes and the copyable Vue adapter
shipped in [0.3.0](../../packages/krm-stream/CHANGELOG.md). Use the
[adoption guide](../adopting.md), [saving guide](../saving.md) and
[conditional editor](../../examples/conditional-save/README.md) for current behavior.

| Priority | Remaining work | Completion evidence |
|---|---|---|
| 1 | Define convergence precisely — complete | [Evidence](../../conformance/README.md#convergence-evidence) |
| 2 | Publish tested deletion-recovery and keep-local recipes | Executed examples preserve unsaved work and unrelated conflicts. |
| 3 | Harden real-API save composition and identity races | Exact-commit API evidence, separate from fake-client CI. |
| 4 | Measure and implement upstream continuation | Same-workload comparison proves continuity, bounded recovery and authorization. |

Review the remaining priorities as separate changes. Baseline measurement can run alongside
priorities 2–3. The normative amendment is complete; see priority 1’s evidence. Adopter-reported
unit tests support adoption, but do not establish real-cluster composition, consumer readiness or
200-attendee capacity.

## 1. Define convergence precisely

Completed: [contract and executable final-write evidence](../../conformance/README.md#convergence-evidence).

## 2. Tested adoption recipes

Keep the existing [user-facing outcomes](../saving.md#what-the-person-editing-sees) and host-owned
[receipt contract](../saving.md#answer-204-or-a-receipt-and-let-the-watch-echo-it) in the saving guide.
The remaining work is executable guidance, using existing store and Vue example tests.

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
`takeTheirs`, then synchronously reapplying it through `setValue` or `removeKey`. Verify the recipe
against current behavior before publishing executable guidance.

**Acceptance:** cover nested paths, deletion, whole-array replacement and policy/redaction refusal;
preserve unrelated edits and conflicts. Capture a fresh save intent after review. Add a small helper
only if repeated consumer code warrants it; do not create another conflict registry.

## 3. Real-API composition and identity races

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
UID replacement in the browser. Voter's 30s recheck / 5s timeout and 60s termination target require
measurement under its actual 200-attendee workload with bounded callbacks and sinks.

Version-only events, independent content/delivery switches, downstream replay, write tickets,
automatic conflict-free retry and a general SSA abstraction remain deferred until a concrete use
case and measurements justify them. Preserve current named-projection semantics and host policy.
