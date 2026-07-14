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

## What the guard rejects

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

## Answer 204 and let the watch echo it

The object a Kubernetes write returns is a *raw* object: `managedFields`, the last-applied annotation,
`status`, and the Secret values your projection withholds. Writing it to the response hands the
browser, through your own save endpoint, exactly what the stream is designed to refuse. The save
endpoint is not covered by the projection unless you cover it.

You do not need to. The write reaches the API server, the watch sees it, and it comes back down the
stream as an ordinary `modified` event: projected, redacted, and three-way merged into the draft the
user is still holding. Dirty state is derived from `draft` versus `server`, so there is nothing to
clear and nothing to adopt. The echo settles it.

## If you must answer with the object

`store.adoptSaved(object)` is for a host that already holds a **projected** object, such as one doing
its own optimistic update. Project it first:

```go
projected, redacted := gateway.Project(gateway.ProjectionFull, result)
_ = redacted // the paths withheld; the client keeps the redactions it already has
writeJSON(w, projected)
```

`gateway.Project` applies the same projection the stream applies. Never hand `adoptSaved` an object
straight from the Kubernetes client.

## Creating and deleting whole objects

A create and a delete are host writes exactly as a save is, and they stay host-side for the same
reasons: RBAC, attribution, and — for a create body — validation all live on the server. The client
stages the *intent*; your endpoint performs the *write*. See
[client state model](client-state-model.md#creating-and-deleting-whole-objects) for the client half —
the store keys on uid and has no merge for these, so the consumer aggregates staged create/delete with
`changes()` into one review list.

```go
// POST /console/configmaps — create
func (s *server) createConfigMap(w http.ResponseWriter, r *http.Request) {
    user := userFromSession(r)
    scope := authorizedScope(user, r)
    object := readObject(r) // the new object the browser assembled

    // Validate on the host, before the write — pin the GVK, the authorized scope and name, and an
    // allowlist of the fields a browser may set. Never trust the assembled object as-is.
    if err := validateCreate(object, scope); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    created, err := s.dynamicFor(user).Resource(configMaps).Namespace(scope.Namespace).
        Create(r.Context(), object, metav1.CreateOptions{})
    if err != nil {
        http.Error(w, "create failed", http.StatusBadGateway)
        return
    }

    // 204 and let the watch echo it — the same recommendation as save. To reflect it now instead,
    // project it first and return it; the browser calls store.adoptSaved(projected).
    _ = created
    w.WriteHeader(http.StatusNoContent)
}

// DELETE /console/configmaps/{name} — delete
func (s *server) deleteConfigMap(w http.ResponseWriter, r *http.Request) {
    user := userFromSession(r)
    scope := authorizedScope(user, r)
    if err := s.dynamicFor(user).Resource(configMaps).Namespace(scope.Namespace).
        Delete(r.Context(), scope.Name, metav1.DeleteOptions{}); err != nil {
        http.Error(w, "delete failed", http.StatusBadGateway)
        return
    }
    // 204; the `deleted` event prunes it from every open stream. To reflect it now instead, the
    // browser calls store.removeResource(uid) with the uid it already tracks.
    w.WriteHeader(http.StatusNoContent)
}
```

`ValidateMergePatch` guards a *patch*. A create sends a whole object, so validate it yourself before
the call — the `validateCreate` above stands in for a schema check or a field allowlist — and pass only
the sanitized object to `Create`. A projected or redacted field must no more ride in on a create body
than in a patch. API-server admission sits behind this as defense in depth, not as a substitute for the
host-side check. A delete carries no body to guard.
