# Examples

The checked browser example in [`vanilla-browser/`](vanilla-browser/) runs against the replay gateway.
For host integration patterns, use these small recipes:

- [Same-origin cookie application](../docs/adopting.md#2-mount-the-same-origin-cookie-endpoint): native
  EventSource, session cookie, dynamic client acting as the user.
- [Bearer-token fetch client](../docs/adopting.md#3-browser-client): `connectResourceStream` with an
  explicit `Authorization` header for a deliberate token-bearing client.
- [Shared backend with SSAR](../docs/adopting.md#4-share-watches-only-with-kubernetes-backed-authorization):
  one service-account watch plus Kubernetes SubjectAccessReviews for each subscriber.
