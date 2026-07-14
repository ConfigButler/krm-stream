# Alternatives and prior art

Where krm-stream sits relative to existing work, and what it does not try to be.

krm-stream is two things: a wire contract for streaming a scoped, redacted projection of KRM
resources into a browser, and a client store that keeps server truth and local drafts separate so a
user can keep typing while the cluster changes underneath them. Most neighbouring projects solve one
half and leave the other to the application.

## Kubernetes client libraries

**[@kubernetes/client-node](https://github.com/kubernetes-client/javascript)** is the official
JavaScript client. It covers watches and informers, including the parts that are easy to get wrong:
`resourceVersion` bookkeeping, bookmarks, `410 Gone` and relist. It is built for Node, speaks
Kubernetes API concepts directly, and ships credential and kubeconfig handling that does not belong
in a browser bundle. It has no notion of a draft, a conflict, or a redacted projection. The
krm-stream gateway sits on this class of library rather than replacing it.

**[kube-watch](https://github.com/subk/kube-watch)** and similar wrappers are the same story with
less coverage: an event emitter over the watch verb, server-side.

**[Raw Kubernetes watch](https://kubernetes.io/docs/reference/using-api/api-concepts/)** gives you
`ADDED`, `MODIFIED`, `DELETED`, bookmarks and streaming initial events. It is the machinery
underneath everything here, not a browser-facing contract. A client still has to solve reconnect,
snapshot completion, history gaps and reconciliation with local state itself. That is the work
krm-stream packages up.

## Browser Kubernetes UIs

**[Headlamp](https://headlamp.dev/)** is the closest architectural precedent for the gateway. Its
browser opens a single WebSocket to `headlamp-server`, which fans out to the cluster API servers.
That is the same posture krm-stream takes with SSE: the API server is never exposed to the browser.
Headlamp also exposes TypeScript APIs (`apiProxy`, `streamResults`, object hooks) to plugin authors.
The differences are that it is a Kubernetes UI and plugin host, its client APIs are React-shaped and
coupled to the Headlamp runtime, and its streaming model is watch-and-replace. Editing is a YAML
editor with `resourceVersion` optimistic concurrency, not a draft that survives a concurrent server
change. If you want a Kubernetes dashboard, use Headlamp. krm-stream is for embedding KRM-backed
live state in an application that is not a Kubernetes UI.

**[@hawtio/kubernetes-api](https://github.com/hawtio/hawtio-kubernetes-api)** is the historical
precedent: browser-side Angular client, WebSocket watch, in-memory collections, CRUD. It talks
directly to the API server and needs CORS configured on the cluster. Its model is to watch and
replace the collection. No drafts, no three-way merge.

**Lens, Skooner, and the Kubernetes Dashboard** are applications, not libraries. Nothing reusable is
published for embedding.

## Config-as-data systems

These share the premise that configuration is data, queryable and mutable through an API, rather
than templates to be rendered. They operate at the package and delivery layer rather than the
live-editing layer.

**[kpt](https://kpt.dev/guides/rationale/) and [Porch](https://github.com/kptdev/porch)** are the
reference Configuration-as-Data implementation, and where the term comes from: configuration data is
the source of truth, stored separately from live state, with the code that acts on it kept out of
the data. They manage the lifecycle of KRM packages in Git, from Draft through Proposed to
Published, with KRM functions mutating packages.

The vocabulary overlaps. Porch has drafts too, but a Porch draft is a package revision moving
through approval gates over minutes or days, not a form field a user is holding while a controller
updates `.status`. Porch is asynchronous and Git-backed. krm-stream is sub-second and
cluster-backed.

**[gitops-reverser](https://reversegitops.dev)** is a complement, not an alternative, and it is why
the krm-stream save boundary looks the way it does. Reverse GitOps puts an API in front and lets Git
remember: a validated write lands on a user-facing CRD, and the accepted intent is recorded to Git
as a manifest, with the actor as commit author, for Flux or Argo CD to distribute. krm-stream is the
read and edit half of that loop. It streams those CRDs into a browser and produces an RFC 7386 merge
patch when the user saves. The two meet at the API: krm-stream never writes, it hands the host a
patch to validate, and the host's write is what reverse GitOps records.

## Local-first and merge libraries

Automerge, Yjs, and the CRDT family solve concurrent editing, and generic JSON-merge libraries solve
structural merging. None of them know what a `resourceVersion` is, that `spec.containers` is keyed
by `name` rather than by index, that a snapshot has a completion point, or that a redacted field
must not be sent back on save. They are ingredients, not alternatives.

The krm-stream merge is deliberately not a CRDT. KRM has one authoritative writer, the API server,
so last-write-wins with the conflict surfaced to the user is the model that matches the data.

**TanStack Query, SWR and Apollo** are the closest analogue in application code: a server cache plus
optimistic updates. They have no streaming Kubernetes source and no snapshot semantics, and their
optimistic update is discarded on refetch, which is the failure krm-stream exists to prevent.

## Summary

| | Streams KRM to browser | Browser-safe (no direct API server) | Framework-independent | Server truth vs. local draft | Conflict-aware three-way merge |
|---|---|---|---|---|---|
| [krm-stream](https://github.com/ConfigButler/krm-stream) | yes | yes (gateway) | yes | yes | yes |
| [@kubernetes/client-node](https://github.com/kubernetes-client/javascript) | no (Node only) | n/a | yes | no | no |
| [Headlamp](https://headlamp.dev/) | yes | yes (headlamp-server) | no (React/plugin host) | no | no |
| [@hawtio/kubernetes-api](https://github.com/hawtio/hawtio-kubernetes-api) | yes | no (CORS to API server) | no (Angular) | no | no |
| [kpt](https://kpt.dev/guides/rationale/) / [Porch](https://github.com/kptdev/porch) | no | n/a | n/a | package drafts, not field drafts | no |
| [Automerge](https://automerge.org/) / [Yjs](https://yjs.dev/) and merge libs | no | n/a | yes | yes | yes, but KRM-unaware |

[gitops-reverser](https://reversegitops.dev) is absent from that table on purpose. It is the write
and record half of the same loop, not a competing way to do this half.

## The claim we can defend

Not "the first Kubernetes streaming library". Watch clients and browser dashboards have existed for
years, and the gateway stands on them.

What is new is the combination: a library for conflict-aware live editing of KRM resources in
browser applications, independent of any UI framework. It is the state layer between Kubernetes
client libraries and application form state. Stated more cautiously: to our knowledge, the first
open browser client and gateway designed for that.
