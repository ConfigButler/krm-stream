# Examples

Start with [watching resources](../docs/why-a-gateway.md) and supported host wiring, then add the
optional editor.

[Native viewer](native-viewer/README.md) is a read-only page that lists and watches native resources
through an existing host proxy, such as `kubectl proxy`, with the shared lifecycle and read-only store.
The checked browser example in [`vanilla-browser/`](vanilla-browser/) runs against the gateway source
over SSE. [Native editor](native-editor/README.md) adds conditional saves through the same proxy,
with machinery protection and guarded native recovery reads, and a minimal page that edits one
ConfigMap with them. Both native pages run in Chromium under `task e2e-browser`, through watch
resumption and expiry recovery.
[Comparison](comparison/README.md) serves the same Widgets and Secrets through native access,
`krm-full/v1` and `krm-spec/v1`, unshared and shared, with editing on every source, host counters and
a measurement driver against the spike cluster; dated results are in
[docs/facts](../docs/facts/comparison-2026-10-05.md).
For gateway host integration patterns, use these small recipes:

- [Same-origin cookie application](../docs/adopting.md#1-mount-the-stream-endpoint): fetch
  connector, session cookie, dynamic client acting as the user.
- [Bearer-token fetch client](../docs/adopting.md#bearer-token-clients): `connectResourceStream` with an
  explicit `Authorization` header for a deliberate token-bearing client.
- [Shared backend with SubjectAccessReview](../docs/adopting.md#5-optional-share-watches-with-kubernetes-backed-authorization):
  one service-account watch plus Kubernetes SubjectAccessReviews for each subscriber.

[Conditional save](conditional-save/README.md) composes a connection with atomic save capture,
projected reconciliation, and a host-owned Kubernetes endpoint that preserves real 409 conflicts.

[Vue adapter](vue/README.md) is a copyable, typechecked composable with reactivity and cleanup tests.

[Editor recipes](editor-recipes/README.md) keep a recovery copy of a draft before its object is
deleted, and resolve a conflict in favour of the local value. Both are executed by the client suite.

[Shared ConfigMap host](../gateway/kube/examples/sharedstream/README.md) is a compiled Go example
with participant SelfSubjectReview, service-account SARs and shared data, bounded SSE writes,
session expiry, lifecycle counters and a manually run real-cluster fixture.
