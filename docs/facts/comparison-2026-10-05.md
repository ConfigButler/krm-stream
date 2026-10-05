# Observed: native, krm-full/v1 and krm-spec/v1 under identical workloads

> **Recorded from runs of the comparison harness on 2026-10-05.** Like
> [shared-host-rehearsal.md](shared-host-rehearsal.md), this is a witness to particular runs on one
> disposable cluster and one machine, not a capacity guarantee, a latency distribution or a support
> statement. Every number below comes from a **real API server**; nothing in this file is simulated.

## What was run

| | |
|---|---|
| Commit | This branch: the resuming native connector and the comparison harness with both gates (the workload gate and the correctness gate including Secret rotation versions); the library under test is this tree's `packages/krm-stream`. The native AFTER section compares it with `d259394`, the last commit that re-lists on every reconnect. |
| Harness | [examples/comparison](../../examples/comparison/README.md): the host in [gateway/kube/examples/comparison](../../gateway/kube/examples/comparison/) and the driver [measure.ts](../../examples/comparison/measure.ts) |
| Command | `task compare-measure` (defaults: workloads quiet, burst, churn, forced-disconnect; N = 1 and 10; 3 repetitions; 20 s per workload; seed 1; 2 forced reconnects per workload; connector `retryDelayMs` 0, `healthyResetMs` 1 s) |
| Library entry | `packages/krm-stream/dist/index.js`, loaded by node 24 |
| Cluster | k3d `v5.9.0`, one server node, k3s `v1.36.4+k3s1` with embedded etcd (`task cluster-up`); `kubectl version`: client `v1.37.0`, server `v1.36.4+k3s1` |
| Toolchain | Go `1.27.1`, Node `v24.21.0`, Docker `29.2.1` (Docker-outside-Docker from the devcontainer) |
| Machine | AMD Ryzen 9 9900X, 16 vCPUs visible to the devcontainer, 47 GiB RAM, Linux `6.17.0-41-generic`. Shared with other work: another agent's test suites may have run against the same cluster during these runs. |
| Host config | Gateway `WriteTimeout` 5 s, `ReauthorizationInterval` 5 s, `ReauthorizationTimeout` 5 s; the host's client-go `QPS` 200 / `Burst` 400 (declared fixture capacity, not a library default); one identity (the kubeconfig's) for every upstream request |
| Objects | 8 Widgets (CRD with a status subresource) and 3 Opaque Secrets (2 keys each) in one scratch namespace |

All five sources run **concurrently** in every run, as N subscribers each, against the same writes:
native, full, spec, full-shared and spec-shared. Each subscriber watches the Widgets and the Secrets
with one connection and one `LiveResourceStore` (read-only policy) per collection. Counts in the
tables cover the **workload phase only** — from the moment every subscriber is live to the end of the
workload — and are **summed over the N subscribers**. Cells are median (min–max) over the 3
repetitions; a single value means all three agreed.

Every run passed two gates before it was counted. The **workload gate**: the host reported no failed
step and did every step it planned, of every kind (`planned` = `done`), so the cluster really changed
as scheduled. The **correctness gate**: after the workload every store held what the cluster held —
for native, each object's exact resourceVersion, spec, status and Secret data; for full and spec, the
same membership and spec (and status for full), no Secret values, exactly the cluster's Secret keys as
redacted paths, and, for every Secret the workload rotated, the resourceVersion of its final
successful rotation. The last check exists because a projected store that missed a rotation still
shows the same redacted key; intermediate rotations may coalesce.

An earlier matrix on this date, with `d259394`'s library and a correctness gate that lacked the
workload and rotation checks, is superseded by the run below; its counts for the gateway sources were
the same.

## Results

Matrix run 2026-10-05 15:39–15:47 UTC (8 min 16 s) with `task compare-measure`: all 24 runs passed
both gates. The refusal check that precedes the matrix passed: each of the four gateway routes ended
the refused session with a terminal `FORBIDDEN`, and the native proxy saw no request from that
session afterwards.

"Writes performed" are the workload's own writes, all of which succeeded; a forced disconnect closes
every downstream native watch and SSE stream of every source at once. For the gateway sources,
`events added` includes the re-snapshot after each forced reconnect (11 objects per subscriber per
reconnect). Native subscribers resume their watches instead, so they receive no re-snapshot.

### quiet

Writes performed (one run): `{"disconnect":2}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 4 | 6.5 (6–9) | 8 (8–13) | 0 | 0 |
| full | 1 | 22 | 0 | 0 | 4 | 22 | 6 (5–6) | 10.3 | 4 | 8 (7.5–11) | 9 (9–17) | 0 | 0 |
| spec | 1 | 22 | 0 | 0 | 4 | 22 | 5 (4–5) | 9.3 | 4 | 7 (7–9) | 9 (8–14) | 0 | 0 |
| full-shared | 1 | 22 | 0 | 0 | 4 | 22 | 5 (5–6) | 10.3 | 4 | 7.5 (7–10) | 9 (9–16) | 0 | 0 |
| spec-shared | 1 | 22 | 0 | 0 | 4 | 22 | 4 (4–5) | 9.3 | 4 | 8 (8–9) | 9 (9–19) | 0 | 0 |
| native | 10 | 0 | 0 | 0 | 0 | 0 | 0 | 0 | 40 | 31 (30.5–36) | 68 (33–75) | 0 | 0 |
| full | 10 | 220 | 0 | 0 | 40 | 220 | 51 (40–65) | 103 | 40 | 42.5 (34.5–63) | 94 (60–176) | 0 | 0 |
| spec | 10 | 220 | 0 | 0 | 40 | 220 | 57 (40–57) | 93.4 | 40 | 38 (37.5–53) | 93 (60–106) | 0 | 0 |
| full-shared | 10 | 220 | 0 | 0 | 40 | 220 | 41 (40–42) | 103 | 40 | 35 (35–59) | 85 (55–88) | 0 | 0 |
| spec-shared | 10 | 220 | 0 | 0 | 40 | 220 | 40 (40–41) | 93.4 | 40 | 41.5 (31.5–58.5) | 93 (44–185) | 0 | 0 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 4 | 0 | 4 | 0 | 0 | 0 | 0 | 2 | 4 |
| native | 10 | 0 | 40 | 0 | 40 | 0 | 0 | 0 | 0 | 20 | 40 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 0 | 10 | 6 | 10.3 | 20 (20–20.1) | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 0 | 10 | 6 | 9.3 | 20 (20–20.1) | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 4 | 10 | 6 | 10.3 | 20 (20–20.1) | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 4 | 10 | 6 | 9.3 | 20 (20–20.1) | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 300 | 0 | 0 | 100 | 60 | 103 | 200 | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 300 | 0 | 0 | 100 | 60 | 93.4 | 200 | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 300 | 0 | 40 | 100 | 60 | 103 | 20 | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 300 | 0 | 40 | 100 | 60 | 93.4 | 20 | 40 |

### burst

Writes performed (one run): `{"disconnect":2,"spec":34,"status":26}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 60 | 0 | 0 | 60 | 60 | 59.8 (59.7–59.8) | 4 | 3 (2.5–3) | 3 | 0 | 0 |
| full | 1 | 22 | 60 | 0 | 4 | 82 | 66 | 38.4 (38.3–38.4) | 4 | 5 (4.5–5.5) | 5 (5–6) | 0 | 0 |
| spec | 1 | 22 | 34 | 0 | 4 | 56 | 40 (40–42) | 23.1 (23.1–23.1) | 4 | 5 | 5 (5–7) | 0 | 0 |
| full-shared | 1 | 22 | 60 | 0 | 4 | 82 | 64 | 38.4 (38.3–38.4) | 4 | 5 (5–6) | 5 (5–7) | 0 | 0 |
| spec-shared | 1 | 22 | 34 | 0 | 4 | 56 | 39 (38–39) | 23.1 (23.1–23.1) | 4 | 4.5 (4.5–5.5) | 6 (5–7) | 0 | 0 |
| native | 10 | 0 | 600 | 0 | 0 | 600 | 600 | 598 (598–598) | 40 | 25 (20–27.5) | 32 (31–35) | 0 | 0 |
| full | 10 | 220 | 600 | 0 | 40 | 820 | 641 (641–644) | 384 (384–384) | 40 | 22.5 (20.5–26) | 33 (29–39) | 0 | 0 |
| spec | 10 | 220 | 340 | 0 | 40 | 560 | 385 (384–388) | 231 (231–231) | 40 | 25 (20–27) | 33 (29–40) | 0 | 0 |
| full-shared | 10 | 220 | 600 | 0 | 40 | 820 | 641 (640–644) | 384 (384–384) | 40 | 22 (17.5–23) | 30 (27–39) | 0 | 0 |
| spec-shared | 10 | 220 | 340 | 0 | 40 | 560 | 380 (380–385) | 231 (231–231) | 40 | 25 (18–27) | 31 (27–39) | 0 | 0 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 4 | 0 | 4 | 60 | 0 | 59.8 (59.7–59.8) | 59.8 (59.7–59.8) | 2 | 4 |
| native | 10 | 0 | 40 | 0 | 40 | 600 | 0 | 598 (598–598) | 598 (598–598) | 20 | 40 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 90 | 0 | 0 | 10 | 6 | 38.4 (38.3–38.4) | 79.8 (79.8–79.9) | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 64 | 26 | 0 | 10 | 6 | 23.1 (23.1–23.1) | 79.8 (79.8–79.9) | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 90 | 0 | 4 | 10 | 6 | 38.4 (38.3–38.4) | 79.8 (79.8–79.9) | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 64 | 26 | 4 | 10 | 6 | 23.1 (23.1–23.1) | 79.8 (79.8–79.9) | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 900 | 0 | 0 | 100 | 60 | 384 (384–384) | 798 (798–800) | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 640 | 260 | 0 | 100 | 60 | 231 (231–231) | 798 (798–800) | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 900 | 0 | 40 | 100 | 60 | 384 (384–384) | 79.8 (79.8–80) | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 640 | 260 | 40 | 100 | 60 | 231 (231–231) | 79.8 (79.8–80) | 40 |

### churn

Writes performed (one run): `{"disconnect":2,"secret":6,"spec":9,"status":199}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 214 | 0 | 0 | 214 | 214 | 211 | 4 | 3.5 (3–4) | 4 (3–4) | 6 | 0 |
| full | 1 | 22 | 214 | 0 | 4 | 236 | 220 (220–221) | 110 | 4 | 5 (3.5–5) | 6 (5–6) | 0 | 6 |
| spec | 1 | 22 | 15 | 0 | 4 | 37 | 22 (21–22) | 15.2 | 4 | 5 (3.5–5) | 5 (5–6) | 0 | 6 |
| full-shared | 1 | 22 | 214 | 0 | 4 | 236 | 218 (218–219) | 110 | 4 | 5 (4–5.5) | 5 (4–6) | 0 | 6 |
| spec-shared | 1 | 22 | 15 | 0 | 4 | 37 | 19 (19–20) | 15.2 | 4 | 4.5 (4–6) | 6 (4–6) | 0 | 6 |
| native | 10 | 0 | 2140 | 0 | 0 | 2140 | 2136 (2136–2140) | 2112 | 40 | 22 (14–26.5) | 26 (19–52) | 60 | 0 |
| full | 10 | 220 | 2138 (2137–2140) | 0 | 40 | 2358 (2357–2360) | 2181 (2176–2181) | 1100 (1100–1101) | 40 | 23 (15.5–28) | 107 (27–113) | 0 | 60 |
| spec | 10 | 220 | 150 | 0 | 40 | 370 | 196 (194–199) | 152 | 40 | 23 (16–28.5) | 106 (26–116) | 0 | 60 |
| full-shared | 10 | 220 | 2140 (2130–2140) | 0 | 40 | 2360 (2350–2360) | 2171 (2170–2183) | 1101 (1097–1101) | 40 | 23 (15–24.5) | 27 (17–61) | 0 | 60 |
| spec-shared | 10 | 220 | 150 | 0 | 40 | 370 | 190 (190–193) | 152 | 40 | 22 (14–24.5) | 27 (18–61) | 0 | 60 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 4 | 0 | 4 | 214 | 0 | 211 | 211 | 2 | 4 |
| native | 10 | 0 | 40 | 0 | 40 | 2140 | 0 | 2112 | 2112 | 20 | 40 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 244 | 0 | 0 | 10 | 6 | 110 | 231 (231–232) | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 45 | 199 | 0 | 10 | 6 | 15.2 | 231 (231–232) | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 244 | 0 | 4 | 10 | 6 | 110 | 231 (231–232) | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 45 | 199 | 4 | 10 | 6 | 15.2 | 231 (231–232) | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 2438 (2437–2440) | 0 | 0 | 100 | 60 | 1100 (1100–1101) | 2315 (2314–2315) | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 450 | 1985 (1984–1990) | 0 | 100 | 60 | 152 | 2311 (2311–2315) | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 2440 (2430–2440) | 0 | 40 | 100 | 60 | 1101 (1097–1101) | 231 (231–232) | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 450 | 1990 | 40 | 100 | 60 | 152 | 232 (231–232) | 40 |

### forced-disconnect

Writes performed (one run): `{"disconnect":6,"secret":18,"spec":24,"status":38}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 80 | 0 | 0 | 80 | 80 | 72.3 | 12 | 3 | 4 (4–5) | 18 | 0 |
| full | 1 | 66 | 80 | 0 | 12 | 146 | 100 (98–103) | 66.7 | 12 | 5 (4.5–5) | 6 (6–10) | 0 | 18 |
| spec | 1 | 66 | 42 | 0 | 12 | 108 | 61 (61–63) | 44.3 | 12 | 5 (4.5–6) | 6 (6–10) | 0 | 18 |
| full-shared | 1 | 66 | 80 | 0 | 12 | 146 | 92 (92–96) | 66.7 | 12 | 5 | 6 (6–11) | 0 | 18 |
| spec-shared | 1 | 66 | 42 | 0 | 12 | 108 | 56 (54–58) | 44.3 | 12 | 5 | 6 (6–11) | 0 | 18 |
| native | 10 | 0 | 800 | 0 | 0 | 800 | 800 | 723 (723–723) | 120 | 21 (18–21) | 29 (25–30) | 180 | 0 |
| full | 10 | 660 | 800 | 0 | 120 | 1460 | 942 (937–948) | 667 (667–667) | 120 | 21 (20–22) | 31 (28–110) | 0 | 180 |
| spec | 10 | 660 | 420 | 0 | 120 | 1080 | 566 (558–569) | 443 (443–444) | 120 | 22 (20–23) | 29 (29–110) | 0 | 180 |
| full-shared | 10 | 660 | 800 | 0 | 120 | 1460 | 925 (923–925) | 667 (667–667) | 120 | 21 (19–22) | 28 (27–28) | 0 | 180 |
| spec-shared | 10 | 660 | 420 | 0 | 120 | 1080 | 546 (544–546) | 443 (443–444) | 120 | 22 (20–22) | 29 (28–110) | 0 | 180 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 0 | 12 | 0 | 12 | 80 | 0 | 72.3 | 72.3 | 2 | 12 |
| native | 10 | 0 | 120 | 0 | 120 | 800 | 0 | 723 (723–723) | 723 (723–723) | 20 | 120 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 12 | 2 | 12 | 0 | 170 | 0 | 0 | 12 | 0 | 66.7 | 134 (133–134) | 12 |
| spec | 1 | 12 | 2 | 12 | 0 | 132 | 38 | 0 | 12 | 0 | 44.3 | 134 (133–134) | 12 |
| full-shared | 1 | 12 | 2 | 12 | 0 | 170 | 0 | 12 | 12 | 0 | 66.7 | 134 (133–134) | 12 |
| spec-shared | 1 | 12 | 2 | 12 | 0 | 132 | 38 | 12 | 12 | 0 | 44.3 | 134 (133–134) | 12 |
| full | 10 | 120 | 20 | 120 | 0 | 1700 | 0 | 0 | 120 | 0 | 667 (667–667) | 1335 (1334–1336) | 120 |
| spec | 10 | 120 | 20 | 120 | 0 | 1320 | 380 | 0 | 120 | 0 | 443 (443–444) | 1335 (1334–1336) | 120 |
| full-shared | 10 | 12 | 2 | 120 | 0 | 1700 | 0 | 120 | 120 | 0 | 667 (667–667) | 134 (133–134) | 120 |
| spec-shared | 10 | 12 | 2 | 120 | 0 | 1320 | 380 | 120 | 120 | 0 | 443 (443–444) | 134 (133–134) | 120 |

## Reading the numbers

Everything here restates the tables; nothing is extrapolated beyond these objects, rates and
subscriber counts.

- **Events.** Native and full delivered the same updates in every workload (churn, N = 1: 214
  `modified` each). Spec delivered 15: the 199 status-only writes were suppressed (`suppressed` = 199)
  and only the 9 spec writes and 6 Secret rotations arrived. Under burst, spec delivered 34 of 60
  updates (the 26 status writes suppressed). At N = 10 full delivered 2138 rather than 2140 in one
  repetition: an update made while a stream was reconnecting arrives inside the next snapshot.
- **Bytes to the browser.** Churn, N = 10: native 2112 KiB, full 1100 KiB, spec 152 KiB. Quiet, where
  only reconnects move anything: native 0 KiB (its watches resume), full 103 KiB and spec 93 KiB (each
  reconnect is a fresh snapshot). The full and spec views remove `managedFields`, Secret values and
  (spec) status, which native carries.
- **Upstream watches and API-server bytes.** The unshared gateway routes open one upstream watch per
  stream per snapshot cycle: 40 at N = 10 in quiet (10 subscribers × 2 collections × 2 reconnects). The
  shared routes opened 4 at both N = 1 and N = 10 — every subscriber left at the same forced
  disconnect, so the shared watch closed and reopened once per collection per reconnect. Under churn at
  N = 10 the unshared routes read about 2311–2315 KiB from the API server and the shared routes 231 KiB.
  The native proxy reads exactly what it forwards (2112 KiB) and opened 40 WATCHes and no LIST: one
  resumed watch per collection per subscriber per reconnect.
- **Reconnects.** Native reconnects resumed in every workload (`LIST` = 0, `WATCH resumed` = `WATCH`,
  no `reset`). The gateway connector requests a fresh snapshot on every reconnect by design (v1 has no
  downstream replay), so the gateway sources re-snapshot every time; the shared routes make that
  cheaper upstream, not downstream. Median time-to-live after a forced disconnect, with no backoff and
  all five sources reconnecting at once: 3–8 ms at N = 1, where native was the lowest in every
  workload (3–6.5 ms); at N = 10, 21–42.5 ms, with no source consistently fastest — 100 connections
  reconnect together, so contention dominates.
- **Authorization work.** The gateway's Authorizer runs per subscriber, shared or not: 100 calls at
  N = 10 in quiet, burst and churn on every route, 60 of them timed rechecks (5 s interval). In
  forced-disconnect no stream stayed live for 5 s, so there were no timed rechecks — only the cycle
  checks. The host's session check runs once per native request (one per resumed WATCH) and once per
  gateway stream request.
- **Secret rotations.** Under churn each native subscriber saw the 6 rotated Secret values change; each
  full and spec subscriber saw 6 redaction revision bumps and never a value, and held the version of
  each Secret's final rotation at the end of every run.

## Native AFTER: resumed watches

Measured 2026-10-05 15:47–16:04 UTC on the same machine and cluster, with

```bash
task compare-native-baseline BASELINE_REF=d259394
```

which built `d259394`'s library (re-list on every reconnect) in a scratch directory, measured the
native source with it, then with this branch's library (`packages/krm-stream` as in the commit
"feat(client): resume native watches from a consumed checkpoint"), using identical defaults: 4
workloads × N 1 and 10 × 3 repetitions × 20 s, 2 forced reconnects per workload (12 in
forced-disconnect), connector `retryDelayMs` 0. Only the native source ran in these two passes, so
time-to-live is not comparable with the five-source tables above. All 48 runs passed both gates:
every workload did all it planned, and every store held each object's exact resourceVersion, spec,
status and Secret data afterwards, with resumed watches and no re-snapshot. Cells are medians over 3
repetitions, summed over the N subscribers; every count except time-to-live was identical across
repetitions, and identical to an earlier pass on this date (14:29–14:46 UTC) made before the
workload and rotation gates existed.

| workload | N | LIST before → after | WATCH resumed | resets | events added | notifications | KiB down (= KiB from API server) | time-to-live median ms |
|---|---|---|---|---|---|---|---|---|
| quiet | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 22 → 0 | 19 → 0 | 6 → 3 |
| quiet | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 220 → 0 | 190 → 0 | 20 → 7.5 |
| burst | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 82 → 60 | 78.8 → 59.8 | 6.5 → 3 |
| burst | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 820 → 600 | 789 → 598 | 16.5 → 6.5 |
| churn | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 236 → 214 | 230 → 211 | 5.5 → 2.5 |
| churn | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 2360 → 2140 | 2302 → 2113 | 13 → 6 |
| forced-disconnect | 1 | 12 → 0 | 0 → 12 | 12 → 0 | 66 → 0 | 146 → 80 | 129 → 72.3 | 7.5 → 3 |
| forced-disconnect | 10 | 120 → 0 | 0 → 120 | 120 → 0 | 660 → 0 | 1460 → 800 | 1295 → 723 | 12 → 5 |

Unchanged in every row: the number of WATCH requests and reconnects, `modified` events and watch
frames (every write still arrives once per subscriber), Secret values seen changing, and active
watches at the end. Host session checks halved (one per resumed WATCH instead of LIST + WATCH).

What this shows, for these objects (11 per subscriber) and rates: a resumed reconnect transfers no
snapshot, so the saving per reconnect is one LIST of the collection and its re-application to the
store — proportional to collection size and reconnect frequency, not to write rate. What it does not
show: reconnects caused by expiry (410), which still re-list by design and were not part of these
workloads — the real-API test `TestRealAPINativeResumeThroughHostProxy` covers that path for
correctness, not cost; reconnects after real API-server watch timeouts (30–60 min) rather than forced
disconnects; larger collections; and backoff, since `retryDelayMs` was 0.

## What this does NOT show

- **API-server-side authorization of native requests.** Kubernetes authorizes every forwarded native
  LIST, WATCH, GET and PATCH as the host's identity, inside the API server. The host cannot see those
  decisions and the harness does not count them. The gateway's `Authorizer` calls are the host's own
  checks, a different thing.
- **Per-caller identities.** Every upstream request uses one identity. A production host serving the
  unshared routes or the native proxy would act as each caller (impersonation or the caller's own
  token); that costs the same watches but adds per-caller credentials, which were not exercised.
- **Browser cost.** The driver is node, not a browser: there is no paint, layout or main-thread
  contention. "Renders" are coalesced macrotasks in which a store notified, not frames. Memory and CPU
  of the browser, the host and the API server were not measured.
- **API-server work.** Upstream bytes and watch counts are measured at the host; watch-cache,
  etcd and serialization cost inside the API server are not.
- **Concurrency effects.** The five sources share one host process, one node process and one cluster in
  every run, so time-to-live includes their contention: at N = 10 each forced disconnect reconnects
  100 connections at once. Run with `--sources <one>` for an isolated figure.
- **Reconnect latency with backoff.** `retryDelayMs` was 0, so time-to-live is reconnect work only. A
  browser with the default 500 ms backoff waits 250–500 ms more before the first retry.
- **Scale.** 11 objects, at most 10 subscribers per source, 20-second workloads, 3 repetitions. No
  percentiles beyond the per-run median and maximum; no soak.
- **Save progress under churn** (proposal 0006, order 5) was not measured. The page exercises editing
  on every source, below; the driver measures viewing only.

## Smoke checks of the page

**Automated since.** These manual checks missed a silent overwrite a reviewer then reproduced: the page
skipped re-rendering a focused input, so after another writer changed a focused, unedited field, the
next keystroke saved the stale text over the newer value with no conflict. The page now follows the
draft in a focused input too, keeping its caret ([fields.ts](../../examples/comparison/fields.ts)).
`task compare-browser` runs that case in Chromium against the real host on both entry points, for
native, full and spec; it passed 6 of 6 and failed 6 of 6 with the previous render rule.
`task test-compare` covers the same rule against the real store without a browser.

Run 2026-10-05 against `task compare` (the host on `https://127.0.0.1:8111/`, HTTP/2, self-signed),
driven headlessly by a throwaway Playwright script in Chromium (the Playwright already installed in
`examples/vanilla-browser`; no spec was added to that suite). **18 of 18 checks passed on each entry
point**, `dist/index.js` (default) and `dist/krm-stream.js` (`?entry=bundle`):

| Check | index | bundle |
|---|---|---|
| Every source live with all 8 Widgets; spec shows no status, full does | pass | pass |
| Native shows Secret values; full and spec show `/data/token ••• rev N`, never a value | pass | pass |
| Native edit saved (`saved`) and echoed on all three sources | pass | pass |
| Full edit saved and echoed; typing in **another** field during the save kept as a dirty draft, no conflict; a second Save writes it | pass | pass |
| Spec edit saved and echoed; typing in the **same** field during the save surfaces as a conflict when the write's own echo arrives; "Keep mine" then Save writes it | pass | pass |
| A competing write conflicts with native and spec drafts; "Keep mine" + Save (native) and "Take theirs" (spec) resolve them | pass | pass |
| A conflicted gateway save is refused with `draft-conflict` | pass | pass |
| "Disconnect all": every source back to live, reconnects counted | pass | pass |
| Refused session: terminal `FORBIDDEN` from the gateway, 0 native requests by that session | pass | pass |
| Shared routes (`?variant=shared` selector) live; host metrics table rendered | pass | pass |
| No page errors other than Chromium's log of the native watches the forced disconnect aborted (`ERR_HTTP2_PROTOCOL_ERROR`) | pass | pass |

The same-field case is the store's documented three-way behavior: the echo moves `spec.note` from
the base to the saved value while the draft holds a later, different value, and the store cannot tell
its own write's echo from anyone else's. Over plain HTTP/1.1 (`http://127.0.0.1:8110/`) the first
native save never completed: the page's six streams held all six connections Chromium allows one
origin. The host's TLS listener exists for that reason.

## Reproducing it

```bash
task cluster-up                   # once
task compare-measure              # this file's matrix: about 9 minutes
task compare-native-baseline BASELINE_REF=d259394   # the native AFTER section: about 17 minutes
task compare-browser              # the page's focused-field regression, both entry points
task compare                      # the page: https://127.0.0.1:8111/
```

`examples/comparison/results/` keeps each run's JSON (every counter of every run) and markdown.
