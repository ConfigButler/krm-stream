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
    result, err := s.dynamicFor(user).Resource(configMaps).Namespace(scope.Namespace).
        Patch(r.Context(), scope.Name, types.MergePatchType, patch, metav1.PatchOptions{})
    writeResult(w, result, err)
}
```

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
