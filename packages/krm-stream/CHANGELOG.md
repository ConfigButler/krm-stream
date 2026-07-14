# Changelog

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
