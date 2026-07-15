# Changelog

## [0.2.1](https://github.com/ConfigButler/krm-stream/compare/gateway/v0.2.0...gateway/v0.2.1) (2026-07-15)


### Miscellaneous Chores

* **gateway:** Synchronize krm-stream versions

## [0.2.0](https://github.com/ConfigButler/krm-stream/compare/gateway/v0.1.1...gateway/v0.2.0) (2026-07-14)


### ⚠ BREAKING CHANGES

* applyStreamEvent returns StreamChange rather than Path[], and onChange receives a StreamChange rather than Path[]. Read `.flashed` for the previous value.
* **gateway:** ScopeFromQuery and ScopePolicy.Validate return error rather than *StreamError. Callers reading .Code directly use errors.As instead.

### Features

* export gateway.Project, and return the whole StreamChange from applyStreamEvent ([#10](https://github.com/ConfigButler/krm-stream/issues/10)) ([915abff](https://github.com/ConfigButler/krm-stream/commit/915abff871d5553c69dd8792c4ab6277dbe50cce))


### Bug Fixes

* **gateway:** return error, not *StreamError, from the exported scope API ([#9](https://github.com/ConfigButler/krm-stream/issues/9)) ([53f7fbd](https://github.com/ConfigButler/krm-stream/commit/53f7fbdffa4fa85184fac5a93658cbfd30d1fc2d))

## [0.1.1](https://github.com/ConfigButler/krm-stream/compare/gateway/v0.1.0...gateway/v0.1.1) (2026-07-14)


### Miscellaneous Chores

* **gateway:** Synchronize krm-stream versions

## 0.1.0 (2026-07-14)


### ⚠ BREAKING CHANGES

* `gateway.RedactedPlaceholder` is removed, and a redacted value is no longer present on the wire in any form. A consumer that rendered the placeholder from the object must render it from `redactedPaths` instead (the TS client exposes `store.redactedPaths(uid)`). No back-compat shim: there are no users yet, and keeping the landmine around to be polite to nobody would be the whole mistake repeated.

### Features

* add KRM resource streaming library ([#1](https://github.com/ConfigButler/krm-stream/issues/1)) ([f415a97](https://github.com/ConfigButler/krm-stream/commit/f415a97023a75b20a88436c483eca564b991fe85))
* seed krm-stream — protocol, conformance corpus, both skeletons ([0f5c65b](https://github.com/ConfigButler/krm-stream/commit/0f5c65b89bd543da51a8636fb6f993c49479c400))
