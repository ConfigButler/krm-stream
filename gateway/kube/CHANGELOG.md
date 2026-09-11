# Changelog

## Unreleased

- Add a tested, copyable shared ConfigMap host example with local full-subject resolution,
  session expiry, service-account SARs, bounded SSE delivery and lifecycle counters. It requires a
  verified-HTTPS cluster configuration, since it carries service and participant tokens. Includes a
  manually invoked independent-identity Kubernetes fixture; no new public identity helper.


## [0.4.0](https://github.com/ConfigButler/krm-stream/compare/gateway/kube/v0.3.0...gateway/kube/v0.4.0) (2026-09-11)


### ⚠ BREAKING CHANGES

* `WriteSSEHeaders` is removed; use `Gateway.ServeStream` or `ServeStreamProjection`, which own headers, delivery and cleanup. `SSESink.Heartbeat` now returns an error and its caller must stop the stream on failure. `NewSSESink(io.Writer)` stays generic and installs no HTTP deadline. The v1 wire protocol is unchanged.
* **kube:** SSARAuthorizer has been removed; use SubjectAccessReviewAuthorizer. The local unscoped krm-stream forwarding package has been removed; use @configbutler/krm-stream.

### Features

* bound HTTP delivery and balance stream lifecycle observations ([#35](https://github.com/ConfigButler/krm-stream/issues/35)) ([6be6cbb](https://github.com/ConfigButler/krm-stream/commit/6be6cbbf63526d0182731489017cbe9c16ad078a))
* **kube:** remove compatibility shims and sharpen stream/save plans ([#31](https://github.com/ConfigButler/krm-stream/issues/31)) ([7477c2d](https://github.com/ConfigButler/krm-stream/commit/7477c2decc719ac7d7ecd55cc79aa3f1edcefd48))


### Documentation

* trim obsolete history and align current integration guidance ([#34](https://github.com/ConfigButler/krm-stream/issues/34)) ([e450334](https://github.com/ConfigButler/krm-stream/commit/e450334ac19c3b39cd546e5a0e6947865afd5e3e))

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
