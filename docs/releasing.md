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

`release.yml` validates the push, then lets Release Please create tags. It resolves the release tags
to one commit and runs the full CI suite on that immutable commit before publishing its exact npm
tarball using npm trusted publishing. Before the first automated release, create the official npm package and configure its
trusted publisher to allow the `ConfigButler/krm-stream` repository's `release.yml` workflow. No
long-lived `NPM_TOKEN` is required after that setup.

Go modules are published by their version tags. The release workflow verifies the new tags from a
clean module consumer after publication.

## Before merging a release PR

```bash
task fixtures-check
task test
task lint
task build-client
```

Confirm that the release PR has passed CI and that the npm trusted-publisher configuration exists
for `@configbutler/krm-stream`.

## Recovery and closely spaced merges

It is safe to merge a release PR while a preceding push is still running CI. Release Please reads
live `main`, so an older run may create the newer commit's tags. Publishing resolves those tags and
validates their commit separately; it never uses the older push's tarball. All three component tags,
the release manifest, the package version, and the artifact's recorded commit must agree.

Publication does not depend on Release Please creating a tag in that run. If GitHub releases exist
but npm publication failed, the next run can recover it. An already published npm version is a
successful no-op. Registry errors fail the run instead of being treated as a missing version, and
recovery refuses to move npm's `latest` tag backwards.

To recover a specific existing release, run the **release** workflow on **main**, optionally setting
`version` (for example, `0.3.0`):

```bash
gh workflow run release.yml --ref main -f version=0.3.0
```

This reruns validation and the Go consumer check before publishing. Do not delete or move release
tags, rename an older tarball, or remove the version/commit checks to recover a failed publication.
