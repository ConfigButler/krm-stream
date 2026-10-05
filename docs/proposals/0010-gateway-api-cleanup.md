# Proposal 0010: Gateway API cleanup

**Status: implemented.** This is the decision record for
PR #54. Use [the gateway README](../../gateway/README.md) for current setup and
[upgrading from 0.7](../migrating.md#go-gateway) for the full migration table.

## Decision

Keep one name per API, share stream configuration and remove repository-only runtime exports before
1.0. Preserve authorization, named projections, bounded HTTP delivery, shared-watch lifecycle and
v1 wire behavior. The gateway remains the projected watch layer and current SSE delivery path;
[native browser transport](../field-reports/third-our-identity.md#native-watch-connector) is separate
requested work. The gateway remains the projected source over SSE; a native viewer does not require
changes to its APIs or framing.

| Area | Implemented result |
|---|---|
| Stream methods | `ServeStream` and transport-neutral `Stream`, each taking the requested projection |
| Shared backend | `NewSharedBackend(upstream, options)` with zero-value-capable options |
| Configuration | `Options` and `Gateway` embed the same `StreamConfig`, using `Authorizer` consistently |
| Projection policy | One `Projections` field; static selection uses `StaticProjection`; nil permits full view only |
| Repository harness | Corpus/fixture/scripted-watch machinery moved into `gateway/internal/conformance` |
| SSE/query details | Private comment helper and heartbeat default; test-owned scope query construction |

Composite literals explicitly name `StreamConfig`; promoted assignments remain available. Empty
requested projection asks the host policy for its default and never bypasses it. Raw Secret disclosure
still requires explicit permission. HTTP validation fails before I/O or backend opening; arbitrary
transport-neutral sinks remain the host's to bound and cancel.

## Capabilities retained

- `Sink`, `Stream` and SSE encoding support distinct host integrations.
- `OrderingLenient` supports known upstreams with unorderable versions, with reduced monotonicity.
- `SLOW_CONSUMER` and reset target fields remain in v1 even when the built-in path does not need them.
- Replay and tests share the fixture interpreter and production encoder; internalizing the harness
  does not replace executable wire evidence with manually written transcripts.

No protocol field or error code was removed. Compatibility changes to the wire require their own
version decision. Browser separation is owned by [proposal 0009](0009-stream-and-editor-separation.md),
and behavioral work by [proposal 0006](0006-stream-and-save-implementation-plan.md).

## Evidence and limits

Focused checks cover policy defaults/refusal, validation, writer capabilities/deadlines, healthy-peer
isolation, shared opening/overflow/backoff/cleanup and internal replay/fixture imports. Runtime change
validation includes fixtures, package/lint, real wire, browser, packed client and Go race checks.
These criteria do not by themselves prove final-commit CI, consumer upgrades or cluster capacity.
See the PR for actual run evidence. Release versions and changelogs remain generated.
