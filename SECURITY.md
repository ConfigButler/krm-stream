# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Report privately through GitHub's
[report a vulnerability](https://github.com/ConfigButler/krm-stream/security/advisories/new) form, which
opens a private advisory only the maintainers can see.

Expect an acknowledgement within 3 working days, and an assessment within 10. If a fix is warranted we
will agree a disclosure date with you and credit you in the advisory unless you would rather we did
not.

## Supported versions

Pre-1.0. Only the latest minor version receives fixes. The protocol and the API may still change.

| Version | Supported |
|---|---|
| 0.1.x | yes |
| < 0.1 | no |

## What counts as a vulnerability here

This library sits between a Kubernetes API server and a browser, so the interesting failures are
almost all *disclosure* failures. The things we would treat as security bugs:

- **A projected or redacted value reaching the browser.** A `Secret` value, `managedFields`, or any
  path the effective projection withheld appearing in a stream event, an error message, or a save
  response. The projection is the boundary; a leak through it is the highest-severity bug this
  codebase can have.
- **A caller receiving an object outside their authorized scope.** Particularly through
  [`SharedBackend`](gateway/shared.go), where one upstream watch is fanned out to many subscribers and
  the host's `Authorizer` is the only thing standing between a caller and the cache. A bug there is
  not a bug, it is a disclosure.
- **A merge patch writing a field the browser was never shown.** `ValidateMergePatch` exists to make
  this impossible; a way around it is a vulnerability, not a feature request.
- **A scope, target or credential accepted from the caller.** The gateway must never let a browser
  choose which API server it talks to.

## What does not

- A host that mounts the gateway without an `Authorizer`, or that skips `ValidateMergePatch` on its
  save endpoint. Both are documented as the host's responsibility, and the first one panics at mount
  time on purpose.
- Anything requiring a Kubernetes credential the browser was never supposed to have.
- Denial of service by a caller who is already authorized to open a watch. Rate limiting is the host's
  edge, not this library's.

## Design notes worth reading first

The boundaries this library claims to hold, and where they are enforced:

- [docs/auth.md](docs/auth.md): identity, RBAC, and the `SharedBackend` trade.
- [docs/saving.md](docs/saving.md): the write path, and why a save answers 204.
- [docs/why-a-gateway.md](docs/why-a-gateway.md): why the browser never holds a cluster credential.
