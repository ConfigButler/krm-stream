# Shared ConfigMap stream host

This compiled example centralizes HTTP delivery and sharing while the host supplies authentication,
credentials and policy. Copy it into your application; its helper functions are not a public kube
identity API. It has no login-service dependency.

Construct `Handler` once at process startup with a service-account `rest.Config`, a fixed namespace
and ConfigMap name, a trusted session resolver, and optionally `&Counters{}`. Mount the returned
handler at the application's stream route. The browser requests:

```text
/resource-stream/v1?version=v1&resource=configmaps&namespace=app&name=coffee
```

The resolver reads the host's authenticated session, validates it, and supplies a participant token,
session expiry and token expiry. It must honor the request context. Browser-supplied identity or
API-server headers are never used by the example. Keep the route behind the host's existing session
protection; do not copy the integration test's fixture-session query routing into production.

`Handler` refuses a cluster configuration that is not HTTPS, and one with `Insecure` set: the
service token and every participant token cross that transport, and `rest.IsConfigTransportTLS`
alone would accept an unverified one. Supply a CA, not a skipped check.

The handler uses the server/TLS settings from the configured cluster for all three operations:

- Participant SelfSubjectReview resolves username, groups, UID and extras at opening. Using the
  service client here would resolve the service account instead. A resolved subject is not a token
  refresh or an authorization grant. The example copies the response data independently.
- Service-account SubjectAccessReviews check list and watch on the fixed scope for that subject,
  before cache disclosure and every 30 seconds. Each check has a five-second callback budget,
  including opening/cycle checks. Changed account/session policy should be checked by the host too.
- One shared service-account backend watches the ConfigMap. A five-second HTTP operation timeout
  bounds each downstream write plus flush. The earlier session/token expiry ends the request.

The service account needs named ConfigMap list/watch and create on `subjectaccessreviews`.
Participants need named list/watch to subscribe; direct GET/PATCH remains participant-authenticated
and separately authorized. Compose with [the conditional-save example](../conditionalsave/handler.go)
and its host CSRF/audit requirements. No write is routed through the shared service identity.

`Counters` updates logical-stream and active-subscription gauges synchronously and counts transport
capability rejections. Unknown observation kinds are ignored. No names, subjects or scope values
become labels. HTTP requests still resolving identity and physical API watch handles are different
lifetimes; keep host instrumentation for them when needed.

## Middleware capability test

Use this pattern in a host test with the **actual middleware chain** mounted around the probe:

```go
checked := make(chan error, 1)
probe := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
    checked <- gateway.CheckHTTPStreaming(w)
    w.WriteHeader(http.StatusNoContent)
})
server := httptest.NewServer(yourMiddleware(probe))
defer server.Close()
response, err := server.Client().Get(server.URL)
if err != nil { t.Fatal(err) }
_ = response.Body.Close()
if err := <-checked; err != nil { t.Fatal(err) }
```

Do not use `httptest.ResponseRecorder` to establish network-deadline support. Do not expose a
production probe endpoint. `CheckHTTPStreaming` writes and flushes nothing, but clears the current
write deadline; call before streaming with no concurrent writer access. It verifies exposed
capabilities, not middleware correctness or proxy buffering. Transparent wrappers need `Unwrap`.
The gateway's [mounted-wrapper test](../../../sse_test.go) exercises success and failure on real
server writers. The gateway's socket tests separately verify stalled-peer behavior.

## Verification and capacity

Normal `go test ./...` in `gateway/kube` compiles this example, checks subject ownership/API failures
and cancellation, and proves that forged browser identity headers cannot change its identity or
scope. The tests use a verified-TLS fake API for the credential boundary, and assert that cleartext
and `Insecure` configurations are refused; they are not Kubernetes RBAC proof. Run the real-cluster test separately against a disposable cluster:

```bash
# From gateway/kube, with the disposable cluster's KUBECONFIG selected:
go test -tags e2e -p 1 -run '^TestSharedHostRealAPI$' -v -count=1 -timeout 4m ./examples/sharedstream
KRM_SHARED_SUBSCRIBERS=200 go test -tags e2e -p 1 -run '^TestSharedHostRealAPI$' -v -count=1 -timeout 4m ./examples/sharedstream
```

The scenario needs exclusive use of the cluster. `apiserver_longrunning_requests` is cluster-wide and
has no namespace label, so any other ConfigMap watcher lands in the same number; run it with `-p 1`
and never alongside the backend e2e suite. One recorded run at 200 identities is in
[docs/facts/shared-host-rehearsal.md](../../../../docs/facts/shared-host-rehearsal.md).

`task test-cluster` also includes the two-identity case. No new cluster CI workflow is required.
The fixture creates isolated RBAC and independent service-account participant tokens, verifies
participant writes, opening/reconnects, one revoked identity with continuing peers, balanced host
gauges, and API-server WATCH requests returning to baseline. It requires administrative fixture
permissions and `/metrics` access; it cleans up its namespace and cluster RBAC. It measures
`apiserver_longrunning_requests` for ConfigMap WATCH requests independently of the library.
Other activity on that resource can invalidate the comparison; use an isolated fixture cluster.

The fixture uses client QPS 100/burst 400 explicitly. At 200 allowed subscriptions and a 30-second
interval, steady authorization is about `2 * 200 / 30 = 13.3` SARs/second, plus opening/recovery
bursts. Simultaneous opening can require 400 SARs and 200 SSRs. Periodic timers may align too.
These rates are a workload description, not universal defaults. Per-client and shared rate limiters,
API-server limits and check latency must be considered together. A larger burst can simply move
contention to the API server.

Record real runs in `docs/facts/` with commit, versions, protocols/proxy, workload, limits, commands,
results and unrun cases. The 30-second recheck, five-second check and 60-second closure target is
not a library guarantee. Gate waiting, in-flight I/O, callback execution and cleanup contribute to
revocation time. A write timeout does not bound backend opening or uncooperative callbacks.

A shared watch still transfers a snapshot to each client and consumes per-client memory, connection
and browser work. Measure sustained load, peak memory/CPU, snapshot bytes and latency distributions
before making production capacity claims. This fixture does not run 200 browser renderers or logins;
expiry and blocked-peer isolation are covered separately by the core HTTP tests.
