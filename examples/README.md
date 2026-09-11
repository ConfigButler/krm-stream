# Examples

The checked browser example in [`vanilla-browser/`](vanilla-browser/) runs against the replay gateway.
For host integration patterns, use these small recipes:

- [Same-origin cookie application](../docs/adopting.md#2-mount-the-same-origin-cookie-endpoint): managed
  fetch stream, session cookie, dynamic client acting as the user.
- [Bearer-token fetch client](../docs/adopting.md#3-browser-client): `connectManagedResourceStream` with an
  explicit `Authorization` header for a deliberate token-bearing client.
- [Shared backend with SubjectAccessReview](../docs/adopting.md#4-share-watches-only-with-kubernetes-backed-authorization):
  one service-account watch plus Kubernetes SubjectAccessReviews for each subscriber.

[Conditional save](conditional-save/README.md) composes a managed connection with atomic save capture,
projected reconciliation, and a host-owned Kubernetes endpoint that preserves real 409 conflicts.

[Vue adapter](vue/README.md) is a copyable, typechecked composable with reactivity and cleanup tests.

[Shared ConfigMap host](../gateway/kube/examples/sharedstream/README.md) is a compiled Go example
with participant SelfSubjectReview, service-account SARs and shared data, bounded SSE writes,
session expiry, lifecycle counters and a manually run real-cluster fixture.
