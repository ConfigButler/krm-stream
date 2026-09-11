# Proposal 0007: Shared-stream transport correctness

**Status: implemented in the working tree; unreleased.** The transport, observation and example
increments below have landed. The real-cluster scenario in increment 3 is manual and has not been
run here, so no `docs/facts/` record exists yet.

## Recommendation

Centralize bounded HTTP delivery and balanced stream/subscription observations in the library.
Keep identity resolution as a tested example and use existing manual cluster tooling for capacity
measurements. The justification is consistent transport and lifecycle behavior across hosts;
line-count reduction is a possible adoption result, not an acceptance criterion.

This responds to Voter's 2026-09-11 request against reported release 0.3.0 and the subsequent
scope review. The initial code review used `e450334`. Voter's unpublished implementation and
200-identity rehearsal have not been reproduced here.

Follow the [design rules](../../CONTRIBUTING.md#design-rules) and
[release policy](../releasing.md). Credentials, sessions, cluster selection, allowlists, RBAC,
writes and metrics export remain host-owned. No wire or browser-store change is planned.

| Increment | Deliver | Keep out |
|---|---|---|
| 1 | HTTP operation deadlines, capability check, transport-rejection observation and cancellation tests | New transport framework or promise of a universal revocation bound. |
| 2 | Stream close plus shared-subscription open/close observations | New observation fields, HTTP request accounting, authorization telemetry and upstream gauges. |
| 3 | Tested shared-host example with local subject resolution and capacity notes | Public identity helper, new cluster CI workflow or adopter-specific login suite. |

[Proposal 0006](0006-stream-and-save-implementation-plan.md) remains independently deliverable.
These changes can help satisfy its existing evidence requirements, but are not additional gates.
Hosts can demonstrate bounded sinks and measurements through their own implementations.

## 1. Bound HTTP delivery

The current [`sse.go`](../../gateway/sse.go) discards flush errors, ignores the emit context,
and stops only the heartbeat goroutine when its write fails. Teardown waits for that goroutine.
In [`reauthorize.go`](../../gateway/reauthorize.go), downstream delivery holds the authorization
gate, and the periodic callback timeout starts after acquiring it. These paths need testing together.

Moving `context.WithTimeout` above `gate.mu.Lock()` alone is not a teardown fix:
[`Mutex.Lock`](https://pkg.go.dev/sync#Mutex.Lock) does not accept cancellation. The budget can
expire while the goroutine still waits, and the in-flight sink write still needs to finish.
Do not ship that move as an independent bounded-revocation guarantee. For this increment, retain
`ReauthorizationTimeout` as the periodic callback budget and bound HTTP I/O at its owner. A
cancelable gate can be considered separately if measurements establish a remaining need; it
would still not terminate an arbitrary blocked sink.

Add `WriteTimeout time.Duration` to `Options` and `Gateway` for HTTP serving only. Zero installs
no library deadline, positive values enable the bound, and negative values are configuration
errors. Validate numeric options at handler construction and before I/O for direct serving calls.
Use five seconds explicitly in the example, not as a universal default.

Implement within the existing SSE transport and reuse it for normal streams and the identity/scope
`refuse` path in [`handler.go`](../../gateway/handler.go):

- One serialized operation gets one absolute deadline covering write plus flush. Include initial
  headers, heartbeat comments and terminal frames. Use an earlier request deadline where present.
- Clear deadlines after successful operations so quiet streams survive many timeout periods.
  Attempt cleanup on exit after transport workers finish. Do not reset a failed transport to
  attempt another frame. Document ownership: a previous host deadline cannot be retrieved and restored.
- Use [`http.ResponseController`](https://pkg.go.dev/net/http#ResponseController) for deadline
  support and error-aware flushing. Propagate reported flush errors and short writes. An errorless
  flusher cannot report every failure; successful flushing is not browser acknowledgement.
- Cancel the internal stream context on header, frame or heartbeat failure and join the heartbeat
  worker. Queued operations must not resume after failure. Never abandon a goroutine in a write.
- Preserve ordinary terminal SSE refusals on supported transports, including error propagation
  from their initial headers and flush.

### Helper and compatibility decisions

Remove exported `WriteSSEHeaders` and replace its serving-path use with an internal error-returning
header operation under the same deadline handling as frames. Update `Handler`, direct serving
entry points and refusal paths together. No compatibility shim; direct HTTP users should use
`Gateway.ServeStream` or `ServeStreamProjection`, which own headers and transport cleanup.

Keep `NewSSESink(io.Writer)` for generic framing, including the existing buffer-based golden tests.
Its argument need not become `http.ResponseWriter`: the HTTP serving path already has that writer
and can supply controller-backed I/O through internal configuration of the same sink. There must
be one frame encoder. Generic callers receive no HTTP timeout guarantee; propagate flush errors
when their writer exposes an error-returning flush method. Change `SSESink.Heartbeat` to return
an error so its HTTP owner can cancel the stream on failure, and update callers to handle it.

The increment's release notes must name removal of `WriteSSEHeaders`, the serving-method migration,
the `Heartbeat` return change, the new timeout/check API and error propagation. These are public
API changes under the pre-1.0 policy, even though no new wire behavior is introduced.

### Unsupported writers

Check deadline/flush capability before committing SSE headers or opening a subscription. The
request's writer does not exist at `Handler` construction, so middleware capability must be tested
at request time and in the host's integration tests. Transparent wrappers should implement `Unwrap`.

Add `CheckHTTPStreaming(w http.ResponseWriter) error`, shared by bounded HTTP serving and host
tests. It checks exposed flush support through the controller-compatible wrapper chain and probes
write-deadline support by setting a zero deadline. It must not write headers/body or invoke flush.
Return an error identifying the missing capability, preserving `errors.Is(err, http.ErrNotSupported)`
for unsupported operations. Call only before streaming with no concurrent writer use: the probe
clears any existing write deadline and cannot restore it. It checks exposed capabilities, not
whether arbitrary middleware correctly implements them or whether a proxy buffers responses.

Add a tested adoption recipe using `httptest.NewServer` with the host's actual middleware chain
around a test-only leaf handler that calls this check and records its result for assertion. Use a
real server writer, not `httptest.ResponseRecorder`, which does not supply network deadlines.
Cover transparent-wrapper success and opaque-wrapper failure. Never mount this probe as a public
production route. Real blocked-client tests remain necessary to establish deadline behavior.

If the host requests a bound and the writer cannot provide it, abort with `http.ErrAbortHandler`.
Do not attempt an unbounded terminal frame: even a tiny frame can block during flush, so that fallback
would violate the requested guarantee. No new wire error code is needed.

This failure can cause browser reconnects and must not be described as a clean terminal SSE refusal.
Emit `http_transport_rejected` once before aborting, using the existing observer and internal-error
code, with no raw middleware error or claim of frame delivery. This event has no corresponding
`stream_opened` and does not change a stream gauge. A broken transport cannot guarantee both bounded
completion and an explanatory frame reaching the browser. This tradeoff is deliberate, not a
solution to automatic reconnects.

### Acceptance

Use real HTTP/1.1 sockets, TLS HTTP/2 and controlled failing writers. Cover a non-reading peer with
enough data to exhaust buffers, quiet/active healthy streams across multiple timeout periods,
header/refusal and heartbeat-only failures, flush errors, short writes, transparent and opaque
wrappers. Verify HTTP/2 isolation on the same connection. Keep these tests in normal Go CI.

Measure expiry, cancellation and periodic revocation during blocked delivery while another
subscriber continues. Assert handler/heartbeat completion, subscriber release and eventual last
upstream cleanup with explicit synchronization and declared timing tolerances.

The write timeout excludes gate waiting, encoding CPU and arbitrary callbacks or backend
`Watch`/`Stop`. Shared opening currently uses a detached context under the backend mutex; do not
claim subscriber expiry bounds it. Generic sinks and host callbacks must cooperate with cancellation.
The total revocation budget still includes timer cadence, in-flight delivery, authorization and cleanup.

## 2. Balance a small set of lifecycle observations

Keep the existing `Observer` interface and `Observation` fields. The complete additions are below;
`http_transport_rejected` ships with increment 1, and the three lifetime events with increment 2.

| Observation | Counting contract |
|---|---|
| `http_transport_rejected` | One pre-stream capability rejection when bounded HTTP serving cannot be supported. No logical stream opens, no close is owed, and no SSE error delivery is attempted. |
| `stream_closed` | Pair existing `stream_opened` at `StreamProjection` entry with one close on return, including authorization failure. It excludes HTTP identity/scope refusals before entry. |
| `shared_subscription_opened` | Report successful attachment to a shared scope, including a warm-cache join. This is a new event; no shared-attach observation exists today. |
| `shared_subscription_closed` | Report once when that attachment becomes inactive through overflow, scope death or leave. A later repeated `Stop` must not count again. |

Retain cycle, overflow, resync, suppression and terminal-error signals. A resnapshot can close and
reopen an attachment on one logical stream without creating a new upstream watch. These counts
are not physical API-server watch counts, HTTP request counts or proof that cleanup has finished.

Preserve `terminal_error` for logical-stream failure. It already occurs before `sink.Emit` in
`emitTerminal`, so it has never guaranteed frame delivery. In increment 1, clarify this in the
`observe.go` comment and the operations alert row, and add the separate transport-rejection row.
Test that unsupported writers emit exactly one rejection, no `stream_opened` or `terminal_error`,
and open no backend; separately test that a failed terminal-frame write still reports the existing
logical-stream failure. Do not count either diagnostic as a lifetime open or close.

Callbacks remain synchronous, prompt and concurrency-safe; no lossy library queue, new IDs or
statistics framework. Guarantee open-before-close for each lifetime, not global ordering. Hosts
update counters synchronously and must not block, panic or reenter the gateway from an observer.
Review callback placement under shared locks. Ignore unknown kinds and avoid turning `Scope`
values into metric labels; correct the overly broad safe-to-export wording in `observe.go`.

**Acceptance:** race-test concurrent/warm-cache joins, denial, revocation, immediate upstream EOF,
recovery, overflow, transport failure and repeated cleanup. Assert intermediate counts and final
balance. Add a small tested counter mapping in the operations example.

Keep HTTP entry/exit and authorization/SAR instrumentation in host wrappers for now. Defer upstream
handle observations too: they can describe a useful library lifetime, but do not measure physical
API-server watches. This smaller change will not let Voter delete all its watch/metrics wrappers;
retain them until equivalent observations are actually provided. Revisit additions when concrete
remaining integration code demonstrates their value, rather than requiring an arbitrary adopter count.

## 3. Test the composition and record capacity

Add `gateway/kube/examples/sharedstream`, following the existing conditional-save example's
placement. Use a local, tested function to issue SelfSubjectReview with a supplied participant
client and copy username, groups, UID and multi-valued extras into `kube.Subject`. Reject missing
username/nil results; preserve API and cancellation errors and independent returned data. No public
resolver API, credential discovery, identity cache, impersonation or guessed OIDC mapping.

Show one process-wide shared backend, trusted session resolution, one server-selected cluster,
fixed namespace/name policy, participant SSR, service-account SAR/data access, explicit write and
periodic-check timeouts, and the earlier session/token expiry. Keep direct writes participant-owned
and link the existing save example. Browser identity headers must not select the client or override
the resolved subject. A service-account SSR resolves the service account; it is not participant identity.

Opening SSR and initial/cycle authorization need explicit short callback deadlines in the example:
`ReauthorizationTimeout` covers periodic checks only. Do not impose a short deadline on the whole
healthy stream. Keep backend request timeouts and session deadlines distinct.

Update [adoption](../adopting.md), [authorization](../auth.md), [operations](../operations.md),
the [gateway README](../../gateway/README.md) and [examples index](../../examples/README.md)
when implementation lands. Explain `2 * N / interval` SARs/second for allowed checks: about 13.3
at 200 subscribers/30 seconds, plus opening/cycle bursts. Two hundred simultaneous openings can
need 400 SARs and 200 separate SSRs. Early denial can issue fewer; periodic checks may align.
Client throttling, shared rate limiters and API-server capacity all matter. Voter's reported
100-QPS/400-burst setting is evidence for its workload, not a default.

Compile and test the example in normal CI, including identity copying, cancellation and host
boundary tests. Reuse manual `task test-cluster`/`task cluster-facts` machinery for a focused load
scenario; add an opt-in Task entry only if needed. No new cluster CI workflow. Record actual runs
in `docs/facts/` with commit, versions, workload, proxy/protocols, limits, commands and results.
Keep general guidance in operations/auth docs and unrun scenarios clearly separate from facts.

Exercise concurrent openings/reconnects, one revoked identity, expiry, a blocked peer, continuing
peers and final cleanup. Use independent API-server evidence for physical watches. The 200-user,
30-second recheck, five-second check and 60-second closure profile from Proposal 0006 is a declared
measurement target, not a guarantee or release-wide load-test requirement. Measure snapshot bytes
and sustained resource/latency behavior; a short rehearsal cannot establish production capacity.
Do not import Voter's login/browser suite or extend Kubernetes support from its k3s 1.31.5 result.

## Delivery

Review transport, observations and example changes separately; observations can proceed independently.
For runtime PRs use the [existing checks](../../CONTRIBUTING.md#test-levels): `task fixtures-check`,
`task test`, `task lint`, focused race tests in changed Go modules, and wire/browser tests for HTTP
changes. Attach a manual real-cluster run for claims about actual SSR/SAR composition; do not label
fake-client results or skipped scenarios as cluster evidence. Planning-only changes need link and
whitespace checks.

Use the lockstep release process and document option defaults, unsupported-writer behavior, flush
errors and event meanings. No release is required just for this proposal. Consumers pin released
artifacts and remove only wrappers whose behavior they have verified is covered; credentials,
scope/session policy and their boundary tests remain in the host.
