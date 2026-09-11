# Alternatives

krm-stream combines a scoped KRM read stream with a browser store for live state, local drafts and
conflicts. Choose tools according to which part of that problem your application needs.

| Need | Relevant approach | Where krm-stream fits |
|---|---|---|
| Kubernetes API access from a server | Kubernetes client libraries and raw watches | The Go adapter uses `client-go`; the gateway adds projections and browser snapshot framing. |
| A complete Kubernetes UI | Dashboard applications and their plugin APIs | krm-stream supplies state and transport; the host builds the UI. |
| Review and deliver configuration packages | KRM package and GitOps systems | The host can record accepted writes in its delivery workflow. |
| Collaborative document editing | CRDT and generic merge libraries | This store reconciles drafts against an authoritative Kubernetes object and surfaces conflicts. |
| Cache server data in a frontend | Query/cache libraries | krm-stream adds the KRM stream lifecycle, redaction metadata and draft reconciliation. |

## Related projects

- [Kubernetes JavaScript client](https://github.com/kubernetes-client/javascript): server-side API
  access. Browser integration still needs host credentials, disclosure policy and stream handling.
- [Headlamp](https://headlamp.dev/): a Kubernetes UI with a plugin system, useful when extending a
  dashboard is the goal.
- [kpt](https://kpt.dev/guides/rationale/) and [Porch](https://github.com/kptdev/porch): KRM package
  workflows. A package revision and an in-progress browser form serve different purposes.
- [Automerge](https://automerge.org/) and [Yjs](https://yjs.dev/): collaborative data structures for
  applications with different synchronization and conflict models.
- [gitops-reverser](https://reversegitops.dev): a complementary write-and-record workflow. The host
  connects accepted Kubernetes writes to Git; krm-stream supplies the read and edit side.

The [architecture overview](../README.md#how-it-fits) shows the library/host split. The
[saving guide](saving.md) describes the conditional-write boundary and its limitations.
