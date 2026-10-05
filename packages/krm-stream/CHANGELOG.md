# Changelog

## [0.9.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.8.0...@configbutler/krm-stream-v0.9.0) (2026-10-05)


### Features

* **examples:** browser page for native editing of one ConfigMap ([#64](https://github.com/ConfigButler/krm-stream/issues/64)) ([0834162](https://github.com/ConfigButler/krm-stream/commit/083416211b652292eb5de39aad5c978e4615ebac))

## [0.8.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.7.0...@configbutler/krm-stream-v0.8.0) (2026-10-05)


### ⚠ BREAKING CHANGES

* **client:** setValue and removeKey on metadata.annotations as a whole are refused; edit each annotation key instead. isEditable on that map is now false.
* **client:** stop the stream when any host callback throws
* **client:** an exception thrown by a subscribe callback or onError now stops the stream without retrying and rejects closed with that exception, as a consumer exception does.
* **client:** connectManagedResourceStream is renamed connectResourceStream(url, consume, options), and the old single-connection connectResourceStream and connectWithEventSource are removed without aliases. ManagedStreamOptions/StreamOptions become ResourceStreamOptions and ManagedStreamHandle/StreamHandle become ResourceStreamHandle; onOpen, onSynced, onGap, onStateChange and onChange are removed in favour of state, subscribe and the callback. applyStreamEvent takes a ResourceStateEvent, without seq or error events. closed now rejects with a callback's exception, and a mismatched X-KRM-Stream-Protocol header is terminal.

### Features

* **client:** deliver resource state events from one stream connector ([#53](https://github.com/ConfigButler/krm-stream/issues/53)) ([d054745](https://github.com/ConfigButler/krm-stream/commit/d0547459594f9c5025aac81c9c5734f7ebfc1f87))
* **client:** native editing through the host proxy (slice 2) ([#61](https://github.com/ConfigButler/krm-stream/issues/61)) ([66f306b](https://github.com/ConfigButler/krm-stream/commit/66f306bbaa3f8b5c0fc4c990f8a793b287a69191))
* **client:** native Kubernetes viewer over LIST/WATCH (slice 1) ([#60](https://github.com/ConfigButler/krm-stream/issues/60)) ([79fb86e](https://github.com/ConfigButler/krm-stream/commit/79fb86e29fb9543418b5594ab03805bae2c2ea5c))


### Bug Fixes

* **client:** never count an adopted save response as snapshot membership ([d7e0677](https://github.com/ConfigButler/krm-stream/commit/d7e0677066cd3433471e15a0d6bd2a5dc4699844))
* **client:** stop the stream when any host callback throws ([d7e0677](https://github.com/ConfigButler/krm-stream/commit/d7e0677066cd3433471e15a0d6bd2a5dc4699844))


### Documentation

* add an upgrade guide from 0.7 ([d7e0677](https://github.com/ConfigButler/krm-stream/commit/d7e0677066cd3433471e15a0d6bd2a5dc4699844))
* **examples:** add tested deletion-recovery and keep-local editor recipes ([#57](https://github.com/ConfigButler/krm-stream/issues/57)) ([939b262](https://github.com/ConfigButler/krm-stream/commit/939b2622e9a3905c364457c6d463508f119ab1a6))
* re-center on watch streams and a native watch direction ([#59](https://github.com/ConfigButler/krm-stream/issues/59)) ([523fd7a](https://github.com/ConfigButler/krm-stream/commit/523fd7a7fbd314472197d106426d6b1a418715e3))

## [0.7.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.6.0...@configbutler/krm-stream-v0.7.0) (2026-10-04)


### Miscellaneous Chores

* **@configbutler/krm-stream:** Synchronize krm-stream versions

## [0.6.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.5.0...@configbutler/krm-stream-v0.6.0) (2026-10-02)


### Features

* refuse credential-carrying redirects, stop reopening early-ending watches, and let releases own versions ([#42](https://github.com/ConfigButler/krm-stream/issues/42)) ([fb52085](https://github.com/ConfigButler/krm-stream/commit/fb52085847e15996ba69fab16f4f021342a20608))

## [0.5.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.4.0...@configbutler/krm-stream-v0.5.0) (2026-10-02)


### ⚠ BREAKING CHANGES

* Principal errors and kube.SubjectAccessReviewAuthorizer's unmapped callers are UNAUTHENTICATED instead of FORBIDDEN; unexpected error text no longer reaches the browser; non-terminal errors other than RESYNC_REQUIRED end the connection.

### Features

* map upstream errors, keep error text off the wire, and let the client own retries ([#40](https://github.com/ConfigButler/krm-stream/issues/40)) ([891e93e](https://github.com/ConfigButler/krm-stream/commit/891e93eed566b76a20ec651b139b30f11691afdc))

### Details

- The managed connector waits at least the server's retry hint (HTTP `Retry-After`, or an error
  event's `retryAfterMs`) before reconnecting, within `maxRetryDelayMs`. A completed snapshot
  discards the hint. `onError` receives it as a fourth argument.
- A refusal whose body is a Kubernetes `Status` reports its `message` instead of `stream: HTTP
  <status>`. The body read is bounded to 16 KiB and 2 seconds, and stops when the stream closes.
- HTTP 429, 502, 503 and 504 report `UPSTREAM_UNAVAILABLE` rather than `INTERNAL`.

## [0.4.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.3.0...@configbutler/krm-stream-v0.4.0) (2026-09-11)


### ⚠ BREAKING CHANGES

* **kube:** SSARAuthorizer has been removed; use SubjectAccessReviewAuthorizer. The local unscoped krm-stream forwarding package has been removed; use @configbutler/krm-stream.

### Features

* **kube:** remove compatibility shims and sharpen stream/save plans ([#31](https://github.com/ConfigButler/krm-stream/issues/31)) ([7477c2d](https://github.com/ConfigButler/krm-stream/commit/7477c2decc719ac7d7ecd55cc79aa3f1edcefd48))


### Bug Fixes

* define content and redaction convergence while suppressed writes retain older RVs ([#33](https://github.com/ConfigButler/krm-stream/issues/33)) ([8b37a92](https://github.com/ConfigButler/krm-stream/commit/8b37a920e95de3485ddc81f0e5a38f66600c3762))

## [0.3.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.2.1...@configbutler/krm-stream-v0.3.0) (2026-09-11)


### Features

* add managed recovery and safe live-editor integration ([#25](https://github.com/ConfigButler/krm-stream/issues/25)) ([df5f97f](https://github.com/ConfigButler/krm-stream/commit/df5f97f758d53e0ab328cb8b333382541a24e07a))

## [0.2.1](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.2.0...@configbutler/krm-stream-v0.2.1) (2026-07-15)


### Documentation

* bless staging whole-object create + delete (edit/create/delete in one review + save) ([#16](https://github.com/ConfigButler/krm-stream/issues/16)) ([a16d803](https://github.com/ConfigButler/krm-stream/commit/a16d8037ca33ea3f7dc338a15e537ab0abe3d697))

## [0.2.0](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.1.1...@configbutler/krm-stream-v0.2.0) (2026-07-14)


### ⚠ BREAKING CHANGES

* applyStreamEvent returns StreamChange rather than Path[], and onChange receives a StreamChange rather than Path[]. Read `.flashed` for the previous value.

### Features

* **client:** publish a single-file browser bundle as ./bundle ([#8](https://github.com/ConfigButler/krm-stream/issues/8)) ([5bbcdae](https://github.com/ConfigButler/krm-stream/commit/5bbcdae8f24be496ada9021ec86565fdd1f65816))
* export gateway.Project, and return the whole StreamChange from applyStreamEvent ([#10](https://github.com/ConfigButler/krm-stream/issues/10)) ([915abff](https://github.com/ConfigButler/krm-stream/commit/915abff871d5553c69dd8792c4ab6277dbe50cce))


### Documentation

* add alternatives, glossary, and why-a-gateway ([#6](https://github.com/ConfigButler/krm-stream/issues/6)) ([d5d686c](https://github.com/ConfigButler/krm-stream/commit/d5d686cd135e000c970517e09b7ed1653d29abf0))

## [0.1.1](https://github.com/ConfigButler/krm-stream/compare/@configbutler/krm-stream-v0.1.0...@configbutler/krm-stream-v0.1.1) (2026-07-14)


### Bug Fixes

* release only the official npm package ([304004e](https://github.com/ConfigButler/krm-stream/commit/304004ed9abada1f99298b6461857e250c960b77))

## 0.1.0 (2026-07-14)


### ⚠ BREAKING CHANGES

* `gateway.RedactedPlaceholder` is removed, and a redacted value is no longer present on the wire in any form. A consumer that rendered the placeholder from the object must render it from `redactedPaths` instead (the TS client exposes `store.redactedPaths(uid)`). No back-compat shim: there are no users yet, and keeping the landmine around to be polite to nobody would be the whole mistake repeated.

### Features

* add KRM resource streaming library ([#1](https://github.com/ConfigButler/krm-stream/issues/1)) ([f415a97](https://github.com/ConfigButler/krm-stream/commit/f415a97023a75b20a88436c483eca564b991fe85))
* seed krm-stream — protocol, conformance corpus, both skeletons ([0f5c65b](https://github.com/ConfigButler/krm-stream/commit/0f5c65b89bd543da51a8636fb6f993c49479c400))
