# Releasing

If npm rejects an upload because the version already exists, the release job checks registry
metadata again for a bounded period. It succeeds only when that version's SHA-512 integrity matches
the validated tarball. Other publish failures, unavailable metadata and mismatched artifacts still
fail. Registry reads request cache revalidation.

The 0.6.0 release demonstrated two distinct delays/failures: its first release attempt failed when
the Go checksum database returned `unknown revision gateway/v0.6.0` shortly after tagging. A rerun
later passed and published npm 0.6.0. A subsequent push attempted the same npm version and was
rejected as a duplicate. Do not bypass Go checksum verification or bump the version to recover these
cases; verify the public release state first. See the
[release run, including its attempts](https://github.com/ConfigButler/krm-stream/actions/runs/37061239061)
and the [duplicate upload failure](https://github.com/ConfigButler/krm-stream/actions/runs/37065350963).

Releases are generated from conventional commits on `main`. Release Please opens a release pull
request with version bumps and changelog entries; merging that pull request creates the release.
Nothing in it needs editing by hand.

The changelogs are written entirely from commit subjects and `BREAKING CHANGE:` footers. Do not add
sections to a `CHANGELOG.md` yourself: Release Please never removes them, so an "Unreleased" section
would ship labelled unreleased. CI refuses one. Put the detail in the commit body or the PR
description, which the changelog links to.

| Commit | Release effect |
|---|---|
| `fix:` | Patch release |
| `feat:` | Minor release |
| `feat!:` or `BREAKING CHANGE:` | Minor before 1.0, major from 1.0 onward |
| `docs:`, `refactor:` | No version bump; included in the changelog |
| `chore:`, `test:`, `ci:`, `build:`, `style:` | No version bump |

## Artifacts

The gateway core, Kubernetes adapter, and official npm client are released in lockstep. A release
produces these tags and packages:

| Artifact | Published as |
|---|---|
| Go gateway | `gateway/vX.Y.Z` tag |
| Go Kubernetes adapter | `gateway/kube/vX.Y.Z` tag |
| Official browser client | `@configbutler/krm-stream` on npm |

Only `@configbutler/krm-stream` is maintained on npm. Do not publish to the unscoped `krm-stream` name.

`gateway/kube/go.mod` requires the core module at the release it ships with. Every release pull
request rewrites that line to the version being released (its `x-release-please-version` marker),
so a released adapter always resolves at least its own core. The matching `replace` in `go.work` moves
with it: a release PR names a core version that is not tagged until it merges, and that replace is
what lets the adapter build in it. `task release-rehearsal`, run by `task verify` and CI, builds the
adapter in exactly that state. Do not edit either line, and do not pin a
pseudo-version when an adapter change needs unreleased core API: `go.work` wires the two modules
together in this checkout, and CI's `a stranger can go get this` job builds an unreleased adapter
with the core from the same commit. Someone using an unreleased commit does the same:

```sh
go get github.com/ConfigButler/krm-stream/gateway/kube@<commit> github.com/ConfigButler/krm-stream/gateway@<commit>
```

Before 1.0, remove superseded API names and forwarding packages instead of maintaining compatibility
shims. Record each removal and its replacement in release notes, and update repository callers,
examples and documentation together. Changes to wire semantics still require explicit spec and
conformance review.

## Publishing setup

`release.yml` follows one path:

1. Release Please creates tags and selects the release commit.
2. CI validates and packs that commit once, including a clean consumer of the public Go tag.
3. npm publishes the tarball produced by that CI run.

If there is no pending npm release, CI checks the triggering push and publishing is skipped.
Tags are created before release CI; npm publication waits for every CI job to pass. A failed CI
run can therefore leave GitHub/Go tags present without an npm package. Fix source problems in a
new release rather than moving public tags.

Configure npm trusted publishing for `ConfigButler/krm-stream`, workflow `release.yml`. No
long-lived `NPM_TOKEN` is required. Configure only the maintained scoped package.

## Before merging a release PR

```bash
task fixtures-check
task test
task lint
task build-client
```

Confirm that the release PR has passed CI and that the npm trusted-publisher configuration exists
for `@configbutler/krm-stream`.

## Retry a failed release

Use **Re-run failed jobs** on the original Actions run, or:

```bash
gh run rerun RUN_ID --failed
```

If npm publication fails, this retries publishing with the **same tested tarball**. Successful
preparation and CI jobs are reused: no new commit, tag, build, or release version is needed. If
npm accepted the upload before the job failed, the retry detects the existing version and succeeds
without publishing again. A failed CI job must pass before publishing can start.

Tarballs are retained for 30 days, matching [GitHub's job retry window](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).
If the artifact was deleted or expired, an explicit recovery run rebuilds and validates once:

```bash
gh workflow run release.yml --ref main -f version=0.3.0
```

Recovery fails on registry errors and refuses to move npm's `latest` backwards. Do not delete or
move release tags, rename an older tarball, or remove the version checks to recover a failure.

## Closely spaced merges

Release Please reads live `main`, so an older push can create a newer release commit's tags.
Preparation resolves all component tags to one immutable commit **before** CI starts. CI checks
out that commit and publishes its artifact, so the triggering push's older package cannot be
mistaken for the release. Publication does not depend on a tag being newly created in that run.
