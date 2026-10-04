# Releasing

Releases are generated from conventional commits on `main`. Release Please opens a release pull
request with version bumps and changelog entries; merging that pull request creates the release.
Nothing in it needs editing by hand.

## Release steps

1. Merge pull requests to `main` with conventional commit titles (see below).
2. Before merging the release pull request, run:

   ```bash
   task fixtures-check
   task test
   task lint
   task build-client
   ```

   and confirm that it has passed CI and that npm trusted publishing is configured for
   `@configbutler/krm-stream`.
3. Merge the release pull request. `release.yml` then:
   1. lets Release Please create the tags and select the release commit;
   2. validates and packs that commit once in CI, including a clean consumer of the public Go tag;
   3. publishes to npm the tarball produced by that CI run.

If a step fails, see [recovering a failed release](#recovering-a-failed-release).

## Commits and changelogs

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

Before 1.0, remove superseded API names and forwarding packages instead of maintaining compatibility
shims. Record each removal and its replacement in release notes, and update repository callers,
examples and documentation together. Changes to wire semantics still require explicit spec and
conformance review.

## Artifacts

The gateway core, Kubernetes adapter, and official npm client are released in lockstep:

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
adapter in exactly that state. Do not edit either line, and do not pin a pseudo-version when an
adapter change needs unreleased core API: `go.work` wires the two modules together in this checkout,
and CI's `a stranger can go get this` job builds an unreleased adapter with the core from the same
commit. Someone using an unreleased commit does the same:

```sh
go get github.com/ConfigButler/krm-stream/gateway/kube@<commit> github.com/ConfigButler/krm-stream/gateway@<commit>
```

## Publishing setup

Configure npm trusted publishing for `ConfigButler/krm-stream`, workflow `release.yml`. No
long-lived `NPM_TOKEN` is required. Configure only the maintained scoped package.

If there is no pending npm release, CI checks the triggering push and publishing is skipped. Tags are
created before release CI, and npm publication waits for every CI job to pass, so a failed CI run can
leave GitHub/Go tags present without an npm package.

Release Please reads live `main`, so an older push can create a newer release commit's tags.
Preparation resolves all component tags to one immutable commit **before** CI starts. CI checks out
that commit and publishes its artifact, so the triggering push's older package cannot be mistaken for
the release. Publication does not depend on a tag being newly created in that run.

## Recovering a failed release

First verify the public release state: which tags exist, whether the Go module resolves, and which
npm version is published. Then:

- **A job failed.** Use **Re-run failed jobs** on the original Actions run, or
  `gh run rerun RUN_ID --failed`. Successful preparation and CI jobs are reused; no new commit, tag,
  build or version is needed. A failed CI job must pass before publishing can start.
- **npm publication failed.** The rerun publishes the **same tested tarball**. If npm accepted the
  upload before the job failed, npm rejects it as a duplicate; the job then rechecks registry
  metadata for a bounded period, with cache revalidation, and succeeds only when that version's
  SHA-512 integrity matches the validated tarball. Other publish failures, unavailable metadata and
  mismatched artifacts still fail.
- **The Go checksum database does not know the new tag yet** (`unknown revision gateway/vX.Y.Z`
  shortly after tagging). Wait and rerun; do not bypass checksum verification.
- **The tarball has expired.** Tarballs are kept for 30 days, matching
  [GitHub's job retry window](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/re-run-workflows-and-jobs).
  An explicit recovery run rebuilds and validates once:

  ```bash
  gh workflow run release.yml --ref main -f version=0.3.0
  ```

  Recovery fails on registry errors and refuses to move npm's `latest` backwards.

Fix source problems in a new release. Do not delete or move release tags, bump the version, rename an
older tarball, or remove the version checks to recover a failure.

The 0.6.0 release hit both delays: its
[first attempt](https://github.com/ConfigButler/krm-stream/actions/runs/37061239061) failed on the
checksum database and passed on rerun, and a
[later push](https://github.com/ConfigButler/krm-stream/actions/runs/37065350963) was rejected as a
duplicate npm upload.
