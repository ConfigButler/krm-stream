# Glossary for frontend developers

Start with [watching resources](why-a-gateway.md); add the [editor](client-state-model.md) when a page
needs drafts. These are the Kubernetes and editing terms used by those guides.

## Resource identity and content

**KRM (Kubernetes Resource Model)** is the JSON shape of a resource: `apiVersion`, `kind`, `metadata`
and kind-specific fields. A ConfigMap uses `data`; many resources have `spec` and `status`; a custom
resource can represent a product's own Database, FeatureFlag or Tenant.

**CRD (Custom Resource Definition)** registers a custom resource kind and its API/schema.

**UID** is the resource's server-assigned identity, used as the store key. A same-name replacement
gets a new UID and never inherits the old draft. Identity is scoped to an upstream **target**: use
one store per stream, or key multiple targets by `(target, uid)`. Names are useful for display/routing,
not for transferring an edit to a replacement.

**Namespace** scopes namespaced resources. Other resource kinds are cluster-scoped. An omitted
namespace on a namespaced collection can mean all namespaces, so the host declares that access.

**Spec and status** commonly distinguish desired configuration from observed progress. The default
editor policy makes status read-only. A read-only field can still follow the server live; it is
omitted only if the selected projection says so.

**Resource version** is a server-assigned token. An object's version is a conditional-write
precondition; a collection/watch version is a recovery checkpoint. They serve different purposes.
Suppression can keep an object's visible content correct while leaving its held write version stale.
See [quiet streams and saving](saving.md#why-a-quiet-stream-can-still-reject-a-save).

## Watching and views

**Watch** is Kubernetes' change feed: added, modified and deleted objects, plus control events such
as bookmarks and errors. History can expire and initialization/recovery must establish a complete
collection boundary. The current gateway handles these mechanics for its browser clients.

**Scope** selects a target, resource kind, namespace, optional name and allowed labels. The host
still authorizes the request; a selector does not grant permission.

**Projection** is the named resource view delivered by the gateway. Full view includes status and
withholds core Secret values; spec view also omits status. Raw includes Secret values with host
permission but still strips managed fields and last-applied configuration. It is not native passthrough.

**Redaction** withholds a value while disclosing its path and change revision. There is no placeholder
in the object to save back accidentally. The UI renders withheld state from redaction metadata and
must not edit it. Built-in Secret redaction does not classify every sensitive CRD field.

**Suppression** skips an object event when projected content excluding resourceVersion plus redaction
records are unchanged. It saves downstream work. A hidden Secret rotation still produces an update.

**Shared watch** uses one upstream watch for matching scopes in a shared backend. It saves duplicate
upstream work; each subscriber still needs authorization, a snapshot and its own delivery.

**Snapshot** establishes complete scope membership. The current gateway emits `reset`, member upserts
and `synced`; only completion prunes unseen UIDs. A partial snapshot cannot establish absence.

**SSE (Server-Sent Events)** is the current gateway's text-event framing, consumed by the official
connector through **fetch**. Browser `EventSource` is another SSE client with different header/retry
limitations. Fetch can read SSE or native Kubernetes watch JSON and inspect HTTP responses; both
formats can carry errors. `connectNativeWatch` reads native watch JSON; `connectResourceStream` reads gateway SSE.

**Gateway** is the embeddable Go read layer. It enforces host scope/view policy, projects resources
and delivers the current SSE protocol. A host can separately proxy native Kubernetes access with a
session cookie while keeping cluster credentials server-side.

## Optional editing and saving

**Server and draft** are separate objects in the editor: the latest delivered authoritative view and
that view with local edits. Store reads are detached copies; render them and edit through store APIs.

**Three-way reconciliation** compares the previous server value (**base**), local draft (**ours**)
and incoming server value (**theirs**) at editable paths:

| Server changed | Local changed | Result |
|---|---|---|
| yes | no | Follow server |
| no | yes | Keep local edit |
| yes | yes, same value | Converge |
| yes | yes, different values | Keep local edit and expose conflict |

The incoming server object becomes the next base. Untouched fields stay live while local work stays
in the draft. A **field conflict** needs explicit review; `revert` takes the current server value.

**Associative list** is a Kubernetes array with schema-declared item keys. With the exact host-supplied
OpenAPI schema, the editor can merge these by key. Other arrays are atomic. The resulting merge patch
still replaces an array as one value.

**Save intent** captures a detached merge patch, UID and resourceVersion together before awaiting a
request. The host authorizes, validates and conditionally writes it. A **merge patch** contains only
editable changes; `null` means deletion. Never write a whole projected object back with PUT.

**Version rejection** means a precondition failed, not necessarily a field disagreement. Current
recovery is a guarded projected read, review and another deliberate Save. A **reconciliation guard**
prevents a delayed read/response from overwriting newer watch state or crossing UID/snapshot recovery.

**Dirty draft, accepted write and domain progress** are separate facts. Later typing survives Save;
204 or a receipt confirms acceptance, while stream observation and rollout/Git completion have their
own meaning. See [saving edits safely](saving.md) for the complete contract and UI outcomes.
