# Proposal 0003: projection-aware patch validation

**Status:** implemented.

## Decision

Keep writes outside `krm-stream`, but provide `gateway.ValidateMergePatch` for host save handlers.
The helper validates an RFC 7386 object merge patch against the active projection and current server
object before the host sends the patch to Kubernetes.

It rejects:

- redacted paths and deletion of a parent that contains one;
- `metadata.managedFields` and the last-applied-configuration annotation;
- `status` when the effective projection is `krm-spec/v1`;
- malformed or non-object merge patches.

## Rationale

Projected objects are intentionally incomplete. A host must never allow a browser to write a field it
was not shown, and should never use whole-object `PUT` for this flow. The helper removes a repeated
high-risk check without taking ownership of authorization, auditing, resource retrieval, or writes.

Secret values are omitted from projected objects rather than represented by placeholders. `redacted`
in the event envelope records their existence without introducing a value that a client could send
back.

See [saving.md](../saving.md) for the host endpoint recipe.
