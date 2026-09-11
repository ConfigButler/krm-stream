# Conditional save with a live draft

This example composes the existing store, managed stream and host-owned writes. It adds no merge
algorithm or shared watch implementation.

- [editor.ts](editor.ts) captures a save intent synchronously, handles HTTP 409 by reconciling a
  projected GET, retains in-flight edits and rejects late responses superseded by the watch.
- [handler.go](../../gateway/kube/examples/conditionalsave/handler.go) is a compilable ConfigMap
  GET/PATCH endpoint. Mount it on a fixed namespace/name with a host-authenticated Kubernetes client
  acting as the caller. The host owns credentials, CSRF protection, route authorization and audit.
- [saving tests](../../packages/krm-stream/test/saving.test.ts) exercise the response races.
- `TestConditionalSaveConflict` in the Kubernetes e2e suite exercises this handler against a real API
  server: a competing update makes a captured save return 409 without overwriting the winner.

```ts
const store = new LiveResourceStore();
const connection = connectManagedResourceStream(streamURL, store, {
  onStateChange: state => renderConnection(state),
});
const editor = conditionalEditor(
  store, uid, "/editor/configmap", hostFetch,
  () => connection.state.status === "live",
);
const unsubscribe = store.subscribe(renderEditor);
// Enable Save only while connection.state.status === "live" and !editor.saving.
// On Save: await editor.save(), then render errors/conflicts and the current draft.
// On unmount:
unsubscribe();
connection.close();
```

Use the same scope and `krm-full/v1` projection on the stream and this example endpoint. The GET
returns a complete projected object with redacted paths; it must use a most-recent Kubernetes read, with
no HTTP or application cache. A recreated name has a different UID and must be opened as a new editor.
Keep an external draft archive if users need to recover edits after deletion: the store intentionally
removes drafts of deleted objects.

A narrow merge patch is **not concurrency protection**. JSON merge patch replaces arrays as a whole,
even when the client merges keyed list items intelligently. This endpoint puts the captured UID and
resourceVersion into the Kubernetes patch. A stale version produces a safe 409, including when
bookkeeping-only changes were suppressed by the full projection. Spec projection also suppresses
status-only updates. This is safe rejection, but sustained churn can hinder save progress. An accepted
GET advances the base without requiring a snapshot. Reconcile first; never transplant an old
patch onto the latest version. No automatic write retry is performed.

This endpoint returns 204 for successful writes. A host may instead return a receipt-only HTTP 200
under the [saving guide’s receipt contract](../../docs/saving.md#answer-204-or-a-receipt-and-let-the-watch-echo-it);
the client example accepts success but leaves receipt parsing to the host. The stream echo settles the saved values while retaining later edits.
If the echo is delayed, dirty state remains visible; prevent repeated saves until your host's chosen
acknowledgment UX allows them. Save results never feed raw Kubernetes objects back into the store.

The GET returns `redactedPaths` from `gateway.Project`, without stream revision counters. The client
preserves revisions for known paths. Unknown redaction paths reject reconciliation; a later
authoritative upsert for that UID or a fresh stream snapshot supplies the missing metadata.
A host with authoritative stream revisions may instead return `redacted`; the editor forwards
that array when `redactedPaths` is absent. If both are supplied, `redactedPaths` takes precedence.
Omitted redaction metadata never clears protection.

The example distinguishes `draft-conflict` (show conflicting paths), `version-stale` (base refreshed,
o field disagreement), `recovering` (a usable base is not established), and `unavailable` (missing or
replacement UID), alongside `unchanged`, `busy` and `saved`. Transport and validation failures throw
host errors. The live-state callback is checked before saving and after reconciliation.

After a refused read, `recovering` preserves the draft. Wait for stream recovery or authoritative
metadata, then the next Save performs only a guarded GET. An accepted read returns `version-stale`
(or actual draft conflicts); a subsequent user action captures a fresh write intent. This conservative
example may perform an extra read when a newer watch already won. It never guesses why the guard
returned false, installs a background retry loop, or retries a PATCH automatically.

Use the [user-facing outcome table](../../docs/saving.md#what-the-person-editing-sees) when wiring the
editor UI. This controller is copyable host code, not a core package export.
