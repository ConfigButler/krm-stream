# Releasing

Versioning is automatic and driven entirely by **conventional commits**. Nobody types a version
number, and nobody pushes a tag.

## The shape of it

Two workflows, and the second one contains the first:

- **`ci.yml`** is the validation pipeline — everything needed to trust a commit, nothing that
  publishes. It runs on every pull request.
- **`release.yml`** runs on pushes to `main`. It *calls* `ci.yml` as a reusable workflow, so the
  release tail runs only after the exact same jobs a PR gets have all gone green in that same run.
  A write token never meets code this pipeline has not already validated.

On each push to `main`, release-please reads the conventional commits since the last release and
keeps a **release PR** open, containing the version bump and the changelog. Merging that PR is the
act of releasing. Nothing else is.

| commit prefix              | effect on the version |
| -------------------------- | --------------------- |
| `fix:`                     | patch                 |
| `feat:`                    | minor                 |
| `feat!:` / `BREAKING CHANGE:` | minor, until 1.0.0 — then major |
| `docs:`, `refactor:`       | no bump (but appears in the changelog) |
| `chore:`, `test:`, `ci:`, `build:`, `style:` | no bump, hidden from the changelog |

While the version is below `1.0.0`, a breaking change bumps the **minor** (`bump-minor-pre-major`),
which is the standard 0.x contract: anything may change, and the minor is where it shows.

## One version, three artifacts

The npm client, the gateway core, and the Kubernetes adapter are released **in lockstep on a single
version number** (release-please's `linked-versions` plugin). That is not laziness — it is the same
claim the repo makes everywhere else: they are one contract and they move in one commit. A client
that speaks protocol N and a gateway that speaks protocol N should not carry version numbers that
need a compatibility table to compare.

So one merged release PR produces four tags:

| tag                       | what it publishes |
| ------------------------- | ----------------- |
| `@configbutler/krm-stream-v0.1.0` | the real npm package, pushed to registry.npmjs.org |
| `krm-stream-v0.1.0`       | the unscoped npm forwarder, pushed after the scoped package |
| `gateway/v0.1.0`          | the Go core module — **the tag *is* the release** |
| `gateway/kube/v0.1.0`     | the Go Kubernetes adapter — likewise |

A Go module is published by nothing more than the existence of a tag named `<module-dir>/vX.Y.Z`.
There is no registry push, which is precisely why `release.yml` ends with **`a stranger can go get
the release`**: it resolves the freshly-created tag from a clean module over the network, with no
checkout, and builds against it. Until something outside this repository does that, "published" is a
hope. (Its sibling in `ci.yml` does the same against a commit sha on every PR, so a mistake is
normally caught long before it reaches a tag.)

The npm side publishes the **exact tarballs `ci.yml` packed and tested** in the same run — the
`client` job runs `npm pack` after the type check, the lint and the build, and `release.yml`
downloads those artifacts and pushes them. What lands on the registry is what this pipeline proved,
not a second build of the same source that merely ought to match.

## npm: trusted publishing, and the one-time setup

There is **no `NPM_TOKEN`**. The `npm` job authenticates over OIDC ("trusted publishing"): npm trades
the workflow's short-lived GitHub identity token for a publish credential. The repo therefore holds
no long-lived registry secret that could leak, and npm attaches SLSA provenance automatically, so the
published package can be traced back to this commit and this workflow.

npm will only let you configure a trusted publisher for a package that **already exists**, so the
very first version has to be bootstrapped by hand — once, ever:

1. Publish `0.1.0` manually (`cd packages/krm-stream && npm publish --access public`, then
   `cd packages/krm-stream-compat && npm publish --access public`), or with a throwaway granular
   token.
2. On npmjs.com → both `@configbutler/krm-stream` and `krm-stream` → **Settings → Trusted
   publisher**, add:
   - repository `ConfigButler/krm-stream`
   - workflow `release.yml`
3. Delete the token if you made one. From here on CI publishes with no secret at all.

## The one thing still to do (after the first release)

`gateway/kube/go.mod` currently requires the core module at a **pseudo-version** — a commit, not a
tag — because until the first release there is no core tag to point at. It resolves fine and it is
what ships today.

Once `gateway/v0.1.0` exists, make it point at the tag and let release-please keep it there:

```diff
- 	github.com/ConfigButler/krm-stream/gateway v0.0.0-20260713074452-d622f2413d1e
+ 	github.com/ConfigButler/krm-stream/gateway v0.1.0 // x-release-please-version
```

...and add the matching updater to `release-please-config.json`, under the `gateway/kube` package:

```json
"extra-files": [{ "type": "generic", "path": "go.mod" }]
```

After that, every release PR bumps the adapter's requirement on the core to the version being
released, and the adapter can never ship pinned to a stale core.

This is deliberately a *second* step rather than part of the initial setup: doing it before a tag
exists would mean release-please rewriting a pseudo-version, and the result of that is not something
worth guessing at. `ci.yml`'s `consumable` job already understands both worlds — if the core version
the adapter requires has no tag yet (which is exactly the state *inside* a release PR), it says so
and defers to the post-tag check in `release.yml`.
