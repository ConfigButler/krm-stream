# Saving edits safely

krm-stream is a read library. Your application owns its HTTP save endpoint, audit policy, and
Kubernetes client. The client store produces a narrow RFC 7386 JSON merge patch; use
`gateway.ValidateMergePatch` immediately before sending it to Kubernetes.

```go
func (s *server) saveConfigMap(w http.ResponseWriter, r *http.Request) {
    user := userFromSession(r)
    scope := authorizedScope(user, r)
    patch := readRequestBody(r)

    // Read the current object using the same identity and effective projection decision as the stream.
    object := s.currentObject(r.Context(), user, scope)
    if err := gateway.ValidateMergePatch(gateway.ProjectionFull, object, patch); err != nil {
        var violation *gateway.PatchViolation
        if errors.As(err, &violation) {
            http.Error(w, violation.Error(), http.StatusBadRequest)
            return
        }
        http.Error(w, "invalid merge patch", http.StatusBadRequest)
        return
    }

    // Your app performs this ordinary Kubernetes PATCH as the signed-in user.
    _, err := s.dynamicFor(user).Resource(configMaps).Namespace(scope.Namespace).
        Patch(r.Context(), scope.Name, types.MergePatchType, patch, metav1.PatchOptions{})
    if err != nil {
        http.Error(w, "save failed", http.StatusBadGateway)
        return
    }

    // 204, and NOT the object Kubernetes just handed back. See below.
    w.WriteHeader(http.StatusNoContent)
}
```

## Answer 204 and let the watch echo it

This is the recommended shape, and it is the one to reach for unless you have a specific reason not
to.

The object returned by a Kubernetes write is a *raw* object: `managedFields`, the last-applied
annotation, `status`, and the Secret values your projection withholds. Writing it to the response
hands the browser, through your save endpoint, precisely what the stream spent its whole design
refusing to send. The save endpoint is not covered by the projection unless you cover it.

You do not need to. The write goes to the API server, the watch sees it, and it arrives back down the
stream as an ordinary `modified` event — projected, redacted, three-way merged into the draft the user
is still holding. The store converges on its own. Dirty state is derived from `draft` versus `server`,
so there is nothing to clear and nothing to adopt: the echo settles it.

## If you must answer with the object

`store.adoptSaved(object)` exists for a host that already holds a **projected** object — a host doing
its own optimistic update, or one that cannot wait a round-trip for the echo. Project it first:

```go
projected, redacted := gateway.Project(gateway.ProjectionFull, result)
_ = redacted // the paths withheld; the client keeps the redactions it already has
writeJSON(w, projected)
```

`gateway.Project` applies the same projection the stream applies. Never hand `adoptSaved` an object
straight from the Kubernetes client.

The guard rejects:

- a value declared in `redacted`, including deletion of a parent map such as `data: null`;
- `metadata.managedFields` and the last-applied-configuration annotation, which every projection
  removes;
- `status` under `krm-spec/v1`;
- a non-object or malformed JSON merge patch.

It does not grant write permission, choose a projection, fetch the object, issue a PATCH, or implement
optimistic concurrency. Those stay with the host. Do not use whole-object `PUT`: projected objects are
intentionally incomplete, and a `PUT` can delete fields the browser never saw.

`metadata.resourceVersion` may be stale when `krm-spec/v1` suppresses invisible status churn. Do not
use the streamed value as a write precondition. The client-side three-way merge surfaces conflicts in
the fields the user can see; send only the user's explicit merge-patch changes.
