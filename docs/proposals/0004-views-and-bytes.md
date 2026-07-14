# Proposal 0004: views, suppression, and redaction revisions

**Status:** accepted for the first public protocol release.

## Decision

The gateway sends a named, host-authorized projection of each Kubernetes object and suppresses an
upstream update when the consumer-visible event is unchanged. The goal is not merely smaller status
events; a consumer that does not render status receives **no event** for status-only churn.

The object remains a strict subset of the API-server object. The gateway may remove values but never
add or replace an object value. Information about removed values belongs in the event envelope, never
in the Kubernetes object.

This proposal intentionally makes a breaking pre-release protocol change:

- `krm-editor/v1` is renamed to `krm-full/v1`.
- `redactedPaths: string[]` is replaced with `redacted: [{ path, rev }]`.
- Every event carries `seq`.

The Go gateway, TypeScript client, schema, fixtures, and documentation move together. There are no
external users to support yet, so retaining two wire shapes would add ambiguity without benefit.

## Projection model

A projection applies one of three actions to each path.

| Action | Value on wire | Path disclosed | Event on change |
|---|---|---:|---:|
| `send` | yes | n/a | yes |
| `redact` | no | yes, in `redacted[]` | yes |
| `ignore` | no | no | no |

`redact` is for a value whose existence and change matter but whose contents must not leave the
gateway. `ignore` is for content the consumer does not render. It is the action that enables zero
traffic for status-only updates.

Built-in projections are deliberately few:

| Projection | `metadata` / `spec` | `status` | Secret values | Intended use |
|---|---|---|---|---|
| `krm-raw/v1` | send | send | send | narrowly scoped operator tooling |
| `krm-full/v1` | send | send | redact | default editor and live-status view |
| `krm-spec/v1` | send | ignore | redact | editor that does not render status |

All projections ignore `metadata.managedFields` and the last-applied-configuration annotation.

The browser may request a projection name, but the host selects the effective projection for the
principal and normalized scope. A browser never supplies projection rules. Projection choice is an
authorization decision because it controls disclosure, particularly for `krm-raw/v1`.

Additional named projections are a later extension. When added, their rules must match `(group, kind)`
before matching a path: a generic `/data` rule would incorrectly redact ConfigMap contents as well as
Secret contents.

## Suppression

After projection and redaction revision calculation, the gateway builds a canonical visible view:

```text
{ object without metadata.resourceVersion, redacted }
```

It hashes that view and emits an `added` or `modified` event only when it differs from the last value
emitted for that UID in the current snapshot cycle. `resourceVersion` stays in the wire object; it is
excluded only from this internal comparison. `seq` is assigned after the suppression decision and is
also excluded.

The suppression map is cleared on every `reset`. A reset marks the consumer's resources unseen, so an
unchanged snapshot object must still be emitted or `synced` would prune it. Redaction revision state
does not reset until the connection ends, allowing a resnapshot to report that a hidden value changed
while upstream continuity was lost.

Suppression is defined over the event envelope, not only the projected object. A Secret rotation can
leave the projected object unchanged; its redaction revision changes, so the event must be emitted.

## Redaction revisions

An upsert always includes `redacted`, including `[]` when nothing is withheld:

```json
{
  "seq": 42,
  "type": "modified",
  "object": { "apiVersion": "v1", "kind": "Secret", "metadata": { "uid": "s1" } },
  "redacted": [{ "path": "/data/token", "rev": 3 }]
}
```

For each `(uid, path)` in one connection:

1. A newly observed redacted path starts at `rev: 1`.
2. Its revision increments when the upstream value changes.
3. A path no longer present upstream disappears from `redacted` and its tracking state is removed.
4. Deletion removes all state for the UID.
5. A reset preserves revisions but a new connection starts fresh.

`rev` says the gateway observed a change since the preceding delivered state. It cannot prove how many
changes occurred while a client was disconnected; a reconnect always starts with a new snapshot.

## Sequence numbers

`seq` is a positive per-connection integer, beginning at one and increasing by one for every event
actually emitted, including `reset`, `synced`, errors, and deletes. It does not reset for a resync
cycle. A new HTTP connection begins at one.

The TypeScript transport checks every sequence number. A missing, repeated, malformed, or out-of-order
number closes the transport and reports a gap; the host reconnects for a fresh snapshot. `seq` is not
an SSE `id` and does not provide replay or resume semantics.

## Consequences

- `krm-spec/v1` makes a status-blind editor cheap under controller churn without weakening the
  complete-object invariant for the fields it receives.
- A suppressed update may leave the consumer's `metadata.resourceVersion` stale. Consumers must not
  use that opaque value as a save precondition. Saves remain narrow merge patches built from local
  edits, while the client-side three-way merge reports visible conflicts live.
- Shared upstream watches remain safe: projection, redaction revision, suppression digest, and sequence
  state are per consumer stream, after upstream fan-out.

## Considered alternatives

- A status digest in the object or envelope still causes an event for every status change. It reduces
  bytes but not wakeups, renders, or reconnect pressure, so it does not solve the motivating case.
- Placeholder values are rejected. A browser can write a placeholder back over a real Secret. Omission
  plus an envelope declaration avoids creating a value that could be round-tripped.
- A status-only projection is not included. Removing `spec` turns the payload into a fragment rather
  than a KRM object. `krm-spec/v1` provides the meaningful traffic reduction without doing that.
