# Releasing

Releases are generated from conventional commits on `main`. Release Please opens a release pull
request with version bumps and changelog entries; merging that pull request creates the release.

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

`krm-stream@0.1.0` is a deprecated, frozen name claim that points to the official package. It is not
part of CI or future releases; new consumers should install `@configbutler/krm-stream`.

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
long-lived `NPM_TOKEN` is required. `krm-stream` is frozen and needs no publisher configuration.

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
