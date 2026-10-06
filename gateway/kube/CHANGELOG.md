# Changelog

## [0.10.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.9.0...gateway/kube/v0.10.0) (2026-10-06)


### Features

* **client:** resume native watches from a consumed checkpoint ([#66](https://github.com/ConfigButler/krm-stream/issues/66)) ([bc8a8fb](https://github.com/ConfigButler/krm-stream/commit/bc8a8fbdbe35cec33cec463bf66bf77d0fd687d3))
* **examples:** compare native, full and spec sources with measurements ([#67](https://github.com/ConfigButler/krm-stream/issues/67)) ([1bad87d](https://github.com/ConfigButler/krm-stream/commit/1bad87d3a3ad28d0d1791cfaf1e580cf390aed0c))

## [0.9.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.8.0...gateway/kube/v0.9.0) (2026-10-05)


### Miscellaneous Chores

* **gateway/kube:** Synchronize krm-stream versions

## [0.8.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.7.0...gateway/kube/v0.8.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **client:** setValue and removeKey on metadata.annotations as a whole are refused; edit each annotation key instead. isEditable on that map is now false.
* **gateway:** Options and Gateway embed StreamConfig, so composite literals set Authorizer, Clients, Projections, Ordering, Observer, Diagnostics, HeartbeatInterval, WriteTimeout and the reauthorization settings inside StreamConfig: gateway.StreamConfig{...}. Gateway.Auth is renamed Authorizer. The Projection field is removed: use Projections: gateway.StaticProjection(p), or leave it nil for ProjectionFull. ServeStream(w, r, principal, scope, requested) and Stream(ctx, principal, scope, requested, sink) replace ServeStreamProjection and StreamProjection; pass "" for the default view. NewSharedBackend(upstream, options) replaces NewSharedBackendWithOptions; pass gateway.SharedOptions{} for the defaults.

### Features

* **client:** native editing through the host proxy (slice 2) ([#61](https://github.com/ConfigButler/krm-stream/issues/61)) ([66f306b](https://github.com/ConfigButler/krm-stream/commit/66f306bbaa3f8b5c0fc4c990f8a793b287a69191))
* **client:** native Kubernetes viewer over LIST/WATCH (slice 1) ([#60](https://github.com/ConfigButler/krm-stream/issues/60)) ([79fb86e](https://github.com/ConfigButler/krm-stream/commit/79fb86e29fb9543418b5594ab03805bae2c2ea5c))
* **gateway:** share one stream configuration and move the test harness out of the gateway ([#54](https://github.com/ConfigButler/krm-stream/issues/54)) ([f39d365](https://github.com/ConfigButler/krm-stream/commit/f39d3650c89f322eae33884fe8fb614819b7c8a5))

## [0.7.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.6.0...gateway/kube/v0.7.0) (2026-10-04)


### ⚠ BREAKING CHANGES

* **gateway:** HTTP hosts using timed reauthorization (Options.ReauthorizationInterval or Gateway.ReauthorizationInterval with ServeStream/ServeStreamProjection) must set a positive per-operation WriteTimeout, or Handler and the direct serving calls panic. Choose a budget per write-plus-flush, count it in the revocation budget, and verify that the mounted middleware supports flushing and write deadlines (gateway.CheckHTTPStreaming in a host test).

### Features

* **gateway:** harden shared-watch opening and timed reauthorization over HTTP ([#50](https://github.com/ConfigButler/krm-stream/issues/50)) ([b18acf4](https://github.com/ConfigButler/krm-stream/commit/b18acf4be96ef753860610e165409fd7e974d0a4))

## [0.6.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.5.0...gateway/kube/v0.6.0) (2026-10-02)


### Features

* refuse credential-carrying redirects, stop reopening early-ending watches, and let releases own versions ([#42](https://github.com/ConfigButler/krm-stream/issues/42)) ([fb52085](https://github.com/ConfigButler/krm-stream/commit/fb52085847e15996ba69fab16f4f021342a20608))

## [0.5.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.4.0...gateway/kube/v0.5.0) (2026-10-02)


### ⚠ BREAKING CHANGES

* Principal errors and kube.SubjectAccessReviewAuthorizer's unmapped callers are UNAUTHENTICATED instead of FORBIDDEN; unexpected error text no longer reaches the browser; non-terminal errors other than RESYNC_REQUIRED end the connection.

### Features

* map upstream errors, keep error text off the wire, and let the client own retries ([#40](https://github.com/ConfigButler/krm-stream/issues/40)) ([891e93e](https://github.com/ConfigButler/krm-stream/commit/891e93eed566b76a20ec651b139b30f11691afdc))

### Details

- Map API-server failures to protocol codes on every path (streaming list, the fallback's LIST and
  WATCH, watch `ERROR` events): 403 `FORBIDDEN` with Kubernetes' message, 401 `UNAUTHENTICATED`,
  404 `SCOPE_INVALID`, 429, 5xx and unreachable servers `UPSTREAM_UNAVAILABLE` with `Retry-After` as
  `retryAfterMs`. A 410 still starts a new snapshot cycle. The raw error, with the URL that failed,
  is the `Cause`.
- A SubjectAccessReview that cannot reach the API server, or is throttled, is `UPSTREAM_UNAVAILABLE`
  and keeps its retry hint.
- **Breaking:** `SubjectAccessReviewAuthorizer` refuses a principal it cannot map with
  `UNAUTHENTICATED` rather than `FORBIDDEN`.

## [0.4.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.3.0...gateway/kube/v0.4.0) (2026-09-11)


### ⚠ BREAKING CHANGES

* `WriteSSEHeaders` is removed; use `Gateway.ServeStream` or `ServeStreamProjection`, which own headers, delivery and cleanup. `SSESink.Heartbeat` now returns an error and its caller must stop the stream on failure. `NewSSESink(io.Writer)` stays generic and installs no HTTP deadline. The v1 wire protocol is unchanged.
* **kube:** SSARAuthorizer has been removed; use SubjectAccessReviewAuthorizer. The local unscoped krm-stream forwarding package has been removed; use @configbutler/krm-stream.

### Features

* bound HTTP delivery and balance stream lifecycle observations ([#35](https://github.com/ConfigButler/krm-stream/issues/35)) ([6be6cbb](https://github.com/ConfigButler/krm-stream/commit/6be6cbbf63526d0182731489017cbe9c16ad078a))
* **kube:** remove compatibility shims and sharpen stream/save plans ([#31](https://github.com/ConfigButler/krm-stream/issues/31)) ([7477c2d](https://github.com/ConfigButler/krm-stream/commit/7477c2decc719ac7d7ecd55cc79aa3f1edcefd48))


### Documentation

* trim obsolete history and align current integration guidance ([#34](https://github.com/ConfigButler/krm-stream/issues/34)) ([e450334](https://github.com/ConfigButler/krm-stream/commit/e450334ac19c3b39cd546e5a0e6947865afd5e3e))

### Details

- Add a tested, copyable shared ConfigMap host example with local full-subject resolution,
  session expiry, service-account SARs, bounded SSE delivery and lifecycle counters. It requires a
  verified-HTTPS cluster configuration, since it carries service and participant tokens. Includes a
  manually invoked independent-identity Kubernetes fixture; no new public identity helper.

## [0.3.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.2.1...gateway/kube/v0.3.0) (2026-09-11)


### Features

* add managed recovery and safe live-editor integration ([#25](https://github.com/ConfigButler/krm-stream/issues/25)) ([df5f97f](https://github.com/ConfigButler/krm-stream/commit/df5f97f758d53e0ab328cb8b333382541a24e07a))

## [0.2.1](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.2.0...gateway/kube/v0.2.1) (2026-07-15)


### Miscellaneous Chores

* **gateway/kube:** Synchronize krm-stream versions

## [0.2.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.1.1...gateway/kube/v0.2.0) (2026-07-14)


### ⚠ BREAKING CHANGES

* **gateway:** ScopeFromQuery and ScopePolicy.Validate return error rather than *StreamError. Callers reading .Code directly use errors.As instead.

### Bug Fixes

* **gateway:** return error, not *StreamError, from the exported scope API ([#9](https://github.com/ConfigButler/krm-stream/issues/9)) ([53f7fbd](https://github.com/ConfigButler/krm-stream/commit/53f7fbdffa4fa85184fac5a93658cbfd30d1fc2d))

## [0.1.1](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.1.0...gateway/kube/v0.1.1) (2026-07-14)


### Miscellaneous Chores

* **gateway/kube:** Synchronize krm-stream versions

## 0.1.0 (2026-07-14)


### ⚠ BREAKING CHANGES

* `gateway.RedactedPlaceholder` is removed, and a redacted value is no longer present on the wire in any form. A consumer that rendered the placeholder from the object must render it from `redactedPaths` instead (the TS client exposes `store.redactedPaths(uid)`). No back-compat shim: there are no users yet, and keeping the landmine around to be polite to nobody would be the whole mistake repeated.

### Features

* add KRM resource streaming library ([#1](https://github.com/ConfigButler/krm-stream/issues/1)) ([f415a97](https://github.com/ConfigButler/krm-stream/commit/f415a97023a75b20a88436c483eca564b991fe85))
