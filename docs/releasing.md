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

`release.yml` runs CI first, then publishes the exact npm tarballs produced by CI using npm trusted
publishing. Before the first automated release, create the official npm package and configure its
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
for both package names.
