# Proposal 0010 Gateway API cleanup

**Status: proposed implementation plan.** No Go APIs are changed by this document.
Source review baseline: `a8281c5`; proposal review branch: `0cde13b`, 2026-10-04.

Simplify gateway configuration and remove duplicate entry points and repository-only harness
exports.
Preserve authorization, projection, bounded HTTP delivery, shared-watch lifecycle and v1 wire
behavior.
Deliver independently from [browser separation](0009-stream-and-editor-separation.md). Follow the
[design rules](../../CONTRIBUTING.md#design-rules) and [release policy](../releasing.md): remove
superseded names before 1.0 and migrate callers together.

The source audit establishes repository use, not every consumer's use. Field reports record
krm-foyer adoption but do not establish which exports its current checkout imports. Inventory
repository callers and available host source during implementation; report unavailable checks.
Published APIs can change before 1.0, with migration notes and no forwarding aliases.

## Selected changes

| Area | Current problem | Result |
|---|---|---|
| Stream methods | Default-view methods forward to projection-aware variants | One HTTP method and one transport-neutral method, each taking a requested projection |
| Shared backend | Default constructor forwards to an options constructor | One constructor with explicit zero-value-capable options |
| Configuration | Options and Gateway repeat fields with manual copying and different authorization names | Shared stream configuration with one name per setting |
| Projection | Projection is ignored when Projections is supplied | One policy field; static selection uses StaticProjection |
| SSE details | Public Comment is used only by a test; heartbeat default docs are ambiguous | Private comment helper and default constant; public configurable interval |
| Repository harness | Production package exports fixtures, loaders and scripted replay | Internal harness used by replay and fixture-driven tests |
| Scope formatting | Scope.Query is used only by a conformance test | Test-owned query construction; public scope parsing retained |

## Stream methods and shared constructor

Keep these names with projection-aware signatures:

```go
func (g *Gateway) ServeStream(
    w http.ResponseWriter, r *http.Request,
    principal Principal, scope Scope, requested Projection,
)

func (g *Gateway) Stream(
    ctx context.Context, principal Principal, scope Scope,
    requested Projection, sink Sink,
) error

func NewSharedBackend(upstream Backend, options SharedOptions) *SharedBackend
```

Empty requested projection asks the configured policy for its default; it never bypasses policy.
Rename existing projection-aware implementations to these names. Remove `ServeStreamProjection`,
`StreamProjection` and `NewSharedBackendWithOptions`. Default callers pass an empty projection or
`SharedOptions{}` respectively. Migrate Handler, replay, tests and kube examples without wrappers.

## Shared stream configuration

Extract shared fields into one exported `StreamConfig`. Options and Gateway embed it:

```go
type Options struct {
    StreamConfig
    Principal func(*http.Request) (Principal, error)
    Scopes ScopePolicy
}

type Gateway struct {
    StreamConfig
    // Existing private runtime/test fields stay here.
}
```

`StreamConfig` contains `Authorizer`, `Clients`, `Projections`, `Ordering`, `Observer`,
`Diagnostics`,
`HeartbeatInterval`, `WriteTimeout`, `ReauthorizationInterval` and `ReauthorizationTimeout`.
Use Authorizer consistently, removing Gateway.Auth. Handler copies the single config value rather
than assigning every shared field individually. Preserve host-callback validation.

Go composite literals must explicitly name the embedded configuration:

```go
gateway.Options{
    Principal: userFromSession,
    Scopes: allowedScopes,
    StreamConfig: gateway.StreamConfig{
        Authorizer: authorizeScope,
        Clients: clientsForUser,
        Projections: gateway.StaticProjection(gateway.ProjectionSpec),
        WriteTimeout: 5 * time.Second,
        ReauthorizationInterval: 30 * time.Second,
        ReauthorizationTimeout: 5 * time.Second,
    },
}
```

Remove the Projection configuration field. Nil Projections retains the safe full policy through
`StaticProjection(ProjectionFull)`. A host with one view supplies StaticProjection; a host selecting
by principal/scope supplies ProjectionPolicy. Raw Secret disclosure continues to require explicit
host policy. Preserve projection authorization during opening, cycles and timed checks.

Preserve validation, including negative values and positive HTTP write timeout with timed checks.
The shared struct must not impose HTTP-only restrictions on transport-neutral sinks. Direct HTTP
validation still occurs before response I/O or backend opening. Update comments and error messages
to identify the shared Ordering setting without assuming Handler users configure a Gateway literal.

## Repository-only exports

Move [conformance.go](../../gateway/conformance.go) and
[scripted.go](../../gateway/scripted.go) into `gateway/internal/conformance` or similarly narrow
internal packages. Move Fixture, WatchOp, FixtureEvent, Corpus, loaders and ScriptedBackend together
as needed. The harness may import gateway domain types; production gateway must not import it.
Use explicit corpus paths from commands/tests. Remove the production relative-path convenience
loader; a test helper may supply the repository path.

A `package gateway` test importing a harness that imports gateway creates a cycle. Move
fixture-driven
conformance and golden tests to `package gateway_test`, including fixture-dependent cases currently
in `stream_test.go`; adapt their recording/replay helpers. Leave private implementation tests in
`package gateway`. Do not convert the entire suite or export private helpers to avoid the cycle.
Inspect shared helper dependencies before editing.

Keep replay and tests on the same fixture interpreter. Preserve use of the production SSE encoder
in golden generation, update flags, protocol.json generation, fixture paths and task names. Do not
replace executable wire evidence with manually encoded transcripts. Core gateway must no longer
include repository corpus filesystem loading in its runtime module graph.

Unexport SSESink.Comment and retain its short-write/failure tests. Rename the heartbeat default to
private `defaultHeartbeatInterval` and document the 20-second default on public config. Move
Scope.Query's only current use into scope conformance test support. Retain ScopeFromQuery and
canonical parser/TypeScript URL checks against the existing corpus.

## Capabilities retained deliberately

| Reviewed removal | Decision and reason |
|---|---|
| SLOW_CONSUMER | Keep Go constant, TypeScript type, schema and spec. Built-in sharing resyncs on overflow, but v1 defines this code for other producers/backends. A teardown test also uses it. No built-in emission does not establish no protocol use. |
| Sink, Stream and SSE encoding APIs | Keep the transport-neutral seam and current framing APIs. They provide distinct capabilities rather than forwarding aliases. Removing them is a separate scope decision. |
| OrderingLenient | Keep explicit support for unorderable aggregated backends. Both strict and lenient behavior have unit tests; the recorded cluster run did not exercise lenient ordering. Removing it changes supported backend behavior. |
| Event.Target on reset | Keep v1 fields and generated output. Redundancy with scope.target is a protocol design question, not justification for a silent removal. |

For generic sinks, clarify the existing requirement that the host bounds delivery and honors
cancellation. Proposal 0008 repaired library-owned HTTP configuration; it did not promise to bound
arbitrary host I/O. Keep generic bounded-sink tests separate from HTTP blocked-reader/deadline
tests.
Deleting the protocol seam is not needed to preserve the HTTP guarantee.

Protocol removals require a separately reviewed compatibility/version decision. This cleanup removes
duplicate configuration and forwarding paths while preserving the protocol and backend contract.

## Implementation order and acceptance

Deliver two changes, each buildable and fully migrated:

| Change | Deliver | Evidence |
|---|---|---|
| 1 | Shared config, projection-policy field, consolidated stream methods and constructor, corrected comments/errors | Handler/direct HTTP and generic sinks preserve policy, validation, deadlines and observations; examples build |
| 2 | Internal corpus/replay harness, test relocation, private Comment/default, test-owned scope query | Fixture/golden output unchanged; replay/browser checks pass; public gateway no longer owns corpus loading |

Update READMEs, adoption, auth and operations guides, root examples and kube examples. Historical
proposals and dated field reports remain historical: update current-guidance links or add a short
migration note when useful, rather than rewriting the APIs they originally reviewed.
[Proposal 0006](0006-stream-and-save-implementation-plan.md) keeps its acceptance criteria and uses
the final method/configuration names in later work.

Run `task fixtures-check`, `task test`, `task lint`, `task e2e-wire`, `task e2e-browser` and
`task pack-client`. Run race checks in both Go modules for changed lifecycle paths. No new cluster
campaign or capacity claim is required for these API/harness changes.

Focused acceptance must establish:

- Nil projection policy redacts Secrets; static policy refuses a different requested view; dynamic
  policy runs at opening, cycles and timed checks.
- Handler/direct HTTP preserve fail-before-I/O validation, unsupported-writer refusal,
  blocked-client
  deadlines and healthy-peer isolation. Generic bounded sinks remain usable.
- Shared-backend zero/explicit options preserve cancellable openings, sharing, overflow recovery,
  backoff, last-subscriber cleanup and balanced observations.
- Both config entry points use the same fields/names without per-field mapping. Current code and
  guidance no longer reference removed public APIs.
- Fixture bytes, protocol.json and canonical query expectations remain unchanged. External fixture
  tests and private tests compile without cycles; replay uses the internal harness.

Use `feat!:` and `BREAKING CHANGE:` notes listing removals, new signatures, nested config literals,
Authorizer naming and StaticProjection migration. Keep lockstep releases and generated changelogs.
Report final tested commit, outcomes, available host checks and unavailable integrations. Complete
this proposal only after those changes land with evidence, independently of proposal 0009.
