# conformance — the shared contract, executable

This directory is the reason `krm-stream` is one repo and not three.

A protocol is only as real as the tests both sides run. Here, **one YAML file describes one scenario
end to end**: what the Kubernetes watch does, what the gateway must therefore put on the wire, and what
a client that consumed that wire (plus some local edits) must then be holding. The Go suite and the
TypeScript suite load the *same* files. A protocol change that breaks either side fails both, in the
same commit.

```
conformance/
  bodies/      real KRM objects, in YAML — the nouns
  fixtures/    scenarios, in YAML — the verbs
  gen/         generated JSON (both suites read this — do not hand-edit)
  generate.sh  bodies+fixtures YAML  ->  gen/bodies.json + gen/fixtures.json
```

**Why YAML in, JSON out.** YAML is what a human reasons about — a `bodies/*.yaml` file is just a
Kubernetes object, the thing you'd `kubectl apply`. JSON is what two languages parse with zero
dependencies (`encoding/json`, `JSON.parse`). So the YAML is the source of truth, `task fixtures`
builds the JSON, and CI fails if the JSON is stale.

---

## The three parts of a fixture

The gateway and the client cannot run the *same* assertions — one produces the wire, the other
consumes it. So a fixture has three sections, and each suite reads the parts that apply to it. They
meet in the middle, at `events`.

```
   watch:    ──▶ [ GATEWAY ] ──▶    events:    ──▶ [ CLIENT ] ──▶  client.expect:
   (input)          Go            (the shared        TS            (draft, dirty,
                                   contract)                        conflicts, patch)
```

| section | who runs it | meaning |
|---|---|---|
| `watch` | gateway | a scripted fake Kubernetes watch. **The gateway's input.** No cluster needed |
| `events` | **both** | the exact wire. The gateway must *emit* this; the client is *fed* this. **This is the contract** |
| `client` | client | local edits applied at points in the stream, and what the store must then hold |

A fixture may omit `client` (a pure stream/framing test) or omit `watch` (a pure merge test, fed
straight from `events`). `suites:` says which suites must run it.

## Anatomy

```yaml
id: edit-vs-unrelated-change              # unique; matches the filename
title: A server change to a key you are NOT editing must not disturb the key you ARE.
why: R-THREEWAY — the base is the PREVIOUS server object, not the draft.
suites: [gateway, client]
scope: { target: demo, version: v1, resource: configmaps, namespace: app }
projection: krm-editor/v1

watch:                                    # ops: list | added | modified | deleted | relist | disconnect
  - { op: list, bodies: [cm-app.v1] }
  - { op: modified, body: cm-app.v2 }

events:                                   # `body:` is a reference into bodies/ — resolved by the loader
  - { type: reset }
  - { type: added, body: cm-app.v1 }
  - { type: synced }
  - { type: modified, body: cm-app.v2 }

client:
  edits:
    - { after: 2, op: set, uid: cm-app-0001, path: [data, log-level], value: debug }
  expect:
    dirty:     [[data, log-level]]        # exact set of dirty paths
    conflicts: []                         # exact set
    draftSubset: { data: { log-level: debug, replicas: "5" } }   # deep-subset of the draft
    patch:     { data: { log-level: debug } }                    # exact RFC 7386 merge patch
```

**`after: N`** applies the edit once event index `N` (0-based) has been processed — so an edit can be
placed *before* the server change that will collide with it. That ordering is the whole point of a
three-way merge, and a fixture format that couldn't express it would be useless.

**`uid:`** — bodies use readable uids (`cm-app-0001`) rather than real UUIDs. A Kubernetes uid is an
opaque string; making it legible costs nothing and makes a delete-and-recreate fixture (§`uid` changes,
name does not) obvious at a glance.

**Assertions.** `dirty` and `conflicts` are compared as exact sets (order-insensitive). `patch` is
compared exactly — it is what gets sent to the API server, so "close enough" is not a thing.
`draftSubset` is a deep-subset check: it lets a fixture assert the two fields it cares about without
restating a whole Deployment.

## The rules these encode

Every fixture names the rule it defends, in `why:`. The ones that catch real bugs:

| fixture | defends |
|---|---|
| `snapshot-then-deltas` | the basic cycle: `reset` … `added` … `synced`, then live deltas |
| `named-object-absent` | a named scope that does not exist is still framed `reset`, `synced` — *not* silence. Skip this and a delete-while-disconnected leaves a ghost forever |
| `reconnect-prune` | pruning is gated on `synced`; a reconnect removes what vanished while away |
| `partial-cycle-no-prune` | a cycle that never reaches `synced` prunes **nothing** |
| `delete-recreate-uid` | identity is `uid`, never `name`; no state bleeds across a recreate |
| `resync-midstream` | upstream continuity can be lost *without* the SSE connection dropping → a fresh cycle mid-stream |
| `nested-field-removed` | `added`/`modified` **replace**; a deep-merge would resurrect a field the server deleted (a ghost) |
| `status-only-churn` | `status` is read-only: it follows the server live, and never becomes dirty, never conflicts, never enters a patch |
| `edit-vs-unrelated-change` | **R-THREEWAY** — the base is the previous *server* object |
| `conflict-and-converge` | a conflict clears when the server's value arrives at what you typed |
| `dotted-label-keys` | **R-ID** — `app.kubernetes.io/name` is ONE path segment. Dot-joining it is silently wrong |
| `array-atomic-on-change` | arrays merge atomically when lengths change (engine spec §4.1); a positional merge mis-aligns |
| `secret-redaction` | a redacted value is never displayed, never dirty, and can never be written back over the real one |

## Adding a fixture

1. Add or reuse a body in `bodies/` (it is a plain Kubernetes object — write it as you would apply it).
2. Add `fixtures/<id>.yaml`. Say in `why:` which rule it defends; if it doesn't defend one, ask whether
   it earns its keep.
3. `task fixtures` (rebuilds `gen/`), then `task test`. Commit the YAML **and** the generated JSON.
