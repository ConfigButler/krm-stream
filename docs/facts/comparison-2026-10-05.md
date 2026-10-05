# Observed: native, krm-full/v1 and krm-spec/v1 under identical workloads

> **Recorded from runs of the comparison harness on 2026-10-05.** Like
> [shared-host-rehearsal.md](shared-host-rehearsal.md), this is a witness to particular runs on one
> disposable cluster and one machine, not a capacity guarantee, a latency distribution or a support
> statement. Every number below comes from a **real API server**; nothing in this file is simulated.

## What was run

| | |
|---|---|
| Commit | `d259394` (`main`) plus the uncommitted harness from this change; the library under test in the matrix below is `d259394`'s `packages/krm-stream` (native connector re-lists on every reconnect). The native AFTER section measures the resuming connector. |
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

Every run passed the correctness gate before it was counted: after the workload every store held
what the cluster held — for native, each object's exact resourceVersion, spec, status and Secret
data; for full and spec, the same membership and spec (and status for full), no Secret values, and
exactly the cluster's Secret keys as redacted paths.

## Results

Matrix run 2026-10-05 14:18–14:26 UTC (8 min 16 s), all 24 runs passed the correctness gate. The refusal check that precedes the matrix passed: each of the four gateway routes ended the refused session with a terminal `FORBIDDEN`, and the native proxy saw no request from that session afterwards.

"Writes performed" are the workload's own writes; a forced disconnect closes every downstream native watch and SSE stream of every source at once. `events added` includes the re-snapshot after each forced reconnect (11 objects per subscriber per reconnect).

### quiet

Writes performed (one run): `{"disconnect":2}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 22 | 0 | 0 | 4 | 22 | 4 | 19 | 4 | 10 (9.5–12) | 10 (10–105) | 0 | 0 |
| full | 1 | 22 | 0 | 0 | 4 | 22 | 5 (4–6) | 10.3 | 4 | 7.5 (7–9) | 10 (8–12) | 0 | 0 |
| spec | 1 | 22 | 0 | 0 | 4 | 22 | 5 (5–6) | 9.3 | 4 | 7 (6.5–10.5) | 8 (7–107) | 0 | 0 |
| full-shared | 1 | 22 | 0 | 0 | 4 | 22 | 4 | 10.3 | 4 | 7.5 (6.5–9.5) | 9 (8–106) | 0 | 0 |
| spec-shared | 1 | 22 | 0 | 0 | 4 | 22 | 4 (4–5) | 9.3 | 4 | 8 (6–10) | 8 (8–107) | 0 | 0 |
| native | 10 | 220 | 0 | 0 | 40 | 220 | 40 | 190 | 40 | 45.5 (37.5–49) | 54 (42–56) | 0 | 0 |
| full | 10 | 220 | 0 | 0 | 40 | 220 | 45 (43–49) | 103 | 40 | 34 (31–34) | 47 (36–52) | 0 | 0 |
| spec | 10 | 220 | 0 | 0 | 40 | 220 | 44 (41–46) | 93.4 | 40 | 34 (31.5–42.5) | 44 (37–53) | 0 | 0 |
| full-shared | 10 | 220 | 0 | 0 | 40 | 220 | 41 (40–42) | 103 | 40 | 32 (26–32.5) | 47 (36–49) | 0 | 0 |
| spec-shared | 10 | 220 | 0 | 0 | 40 | 220 | 41 (40–41) | 93.4 | 40 | 31 (26–35) | 45 (35–53) | 0 | 0 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 4 | 4 | 4 | 0 | 0 | 0 | 19 | 19 | 2 | 8 |
| native | 10 | 40 | 40 | 40 | 0 | 0 | 0 | 190 | 190 | 20 | 80 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 0 | 10 | 6 | 10.3 | 20 (20–20.1) | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 0 | 10 | 6 | 9.3 | 20 | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 4 | 10 | 6 | 10.3 | 20 | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 30 | 0 | 4 | 10 | 6 | 9.3 | 20 | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 300 | 0 | 0 | 100 | 60 | 103 | 200 (200–201) | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 300 | 0 | 0 | 100 | 60 | 93.4 | 200 (200–201) | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 300 | 0 | 40 | 100 | 60 | 103 | 20 (20–20.1) | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 300 | 0 | 40 | 100 | 60 | 93.4 | 20 (20–20.1) | 40 |

### burst

Writes performed (one run): `{"disconnect":2,"spec":34,"status":26}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 22 | 60 | 0 | 4 | 82 | 64 | 78.8 (78.8–78.8) | 4 | 7 (6–10) | 8 (7–12) | 0 | 0 |
| full | 1 | 22 | 60 | 0 | 4 | 82 | 65 (64–65) | 38.4 (38.3–38.4) | 4 | 5.5 (5.5–8.5) | 7 (6–9) | 0 | 0 |
| spec | 1 | 22 | 34 | 0 | 4 | 56 | 40 | 23.1 (23.1–23.1) | 4 | 5.5 (5–8) | 6 (6–8) | 0 | 0 |
| full-shared | 1 | 22 | 60 | 0 | 4 | 82 | 64 (64–65) | 38.4 (38.3–38.4) | 4 | 6.5 (6–6.5) | 7 (6–8) | 0 | 0 |
| spec-shared | 1 | 22 | 34 | 0 | 4 | 56 | 38 (38–39) | 23.1 (23.1–23.1) | 4 | 6 (5–8) | 6 (6–9) | 0 | 0 |
| native | 10 | 220 | 600 | 0 | 40 | 820 | 640 | 789 (789–789) | 40 | 37 (30–42) | 50 (36–130) | 0 | 0 |
| full | 10 | 220 | 600 | 0 | 40 | 820 | 644 (643–648) | 384 (384–384) | 40 | 28 (19.5–30) | 41 (26–135) | 0 | 0 |
| spec | 10 | 220 | 340 | 0 | 40 | 560 | 388 (384–390) | 231 (231–231) | 40 | 28 (20–36) | 46 (27–134) | 0 | 0 |
| full-shared | 10 | 220 | 600 | 0 | 40 | 820 | 642 (641–642) | 384 (384–384) | 40 | 29 (19–31.5) | 44 (27–137) | 0 | 0 |
| spec-shared | 10 | 220 | 340 | 0 | 40 | 560 | 381 (380–381) | 231 (231–231) | 40 | 22.5 (19.5–31) | 48 (27–129) | 0 | 0 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 4 | 4 | 4 | 0 | 60 | 0 | 78.8 (78.8–78.8) | 78.8 (78.8–78.8) | 2 | 8 |
| native | 10 | 40 | 40 | 40 | 0 | 600 | 0 | 789 (789–789) | 789 (789–789) | 20 | 80 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 90 | 0 | 0 | 10 | 6 | 38.4 (38.3–38.4) | 79.8 (79.8–79.9) | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 64 | 26 | 0 | 10 | 6 | 23.1 (23.1–23.1) | 79.8 (79.8–79.9) | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 90 | 0 | 4 | 10 | 6 | 38.4 (38.3–38.4) | 79.8 (79.8–79.9) | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 64 | 26 | 4 | 10 | 6 | 23.1 (23.1–23.1) | 79.8 (79.8–79.9) | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 900 | 0 | 0 | 100 | 60 | 384 (384–384) | 800 (798–800) | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 640 | 260 | 0 | 100 | 60 | 231 (231–231) | 800 (798–800) | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 900 | 0 | 40 | 100 | 60 | 384 (384–384) | 80 (79.8–80) | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 640 | 260 | 40 | 100 | 60 | 231 (231–231) | 80 (79.8–80) | 40 |

### churn

Writes performed (one run): `{"disconnect":2,"secret":6,"spec":9,"status":199}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 22 | 214 | 0 | 4 | 236 | 218 | 230 | 4 | 6 (6–7) | 8 (6–9) | 6 | 0 |
| full | 1 | 22 | 214 | 0 | 4 | 236 | 222 (219–223) | 110 | 4 | 5 (5–6) | 7 (5–9) | 0 | 6 |
| spec | 1 | 22 | 15 | 0 | 4 | 37 | 20 (20–21) | 15.2 | 4 | 6 (5–6) | 6 (6–9) | 0 | 6 |
| full-shared | 1 | 22 | 214 | 0 | 4 | 236 | 219 | 110 | 4 | 6 (5.5–7) | 6 (6–9) | 0 | 6 |
| spec-shared | 1 | 22 | 15 | 0 | 4 | 37 | 19 (19–20) | 15.2 | 4 | 5.5 (5–7.5) | 6 (6–10) | 0 | 6 |
| native | 10 | 220 | 2131 (2130–2140) | 0 | 40 | 2351 (2350–2360) | 2171 (2170–2179) | 2293 (2292–2302) | 40 | 28 (24–31) | 110 (33–110) | 60 | 0 |
| full | 10 | 220 | 2136 (2131–2140) | 0 | 40 | 2356 (2351–2360) | 2178 (2178–2182) | 1099 (1097–1101) | 40 | 21 (20.5–31) | 106 (29–111) | 0 | 60 |
| spec | 10 | 220 | 150 | 0 | 40 | 370 | 195 (193–203) | 152 | 40 | 19 (17–31) | 29 (25–111) | 0 | 60 |
| full-shared | 10 | 220 | 2140 (2134–2140) | 0 | 40 | 2360 (2354–2360) | 2179 (2175–2181) | 1101 (1098–1101) | 40 | 17.5 (14–23) | 26 (21–33) | 0 | 60 |
| spec-shared | 10 | 220 | 150 | 0 | 40 | 370 | 191 (190–192) | 152 | 40 | 16 (16–22.5) | 26 (19–33) | 0 | 60 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 4 | 4 | 4 | 0 | 214 | 0 | 230 | 230 | 2 | 8 |
| native | 10 | 40 | 40 | 40 | 0 | 2131 (2130–2140) | 0 | 2293 (2292–2302) | 2293 (2292–2302) | 20 | 80 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 4 | 2 | 4 | 0 | 244 | 0 | 0 | 10 | 6 | 110 | 232 | 4 |
| spec | 1 | 4 | 2 | 4 | 0 | 45 | 199 | 0 | 10 | 6 | 15.2 | 232 | 4 |
| full-shared | 1 | 4 | 2 | 4 | 0 | 244 | 0 | 4 | 10 | 6 | 110 | 232 | 4 |
| spec-shared | 1 | 4 | 2 | 4 | 0 | 45 | 199 | 4 | 10 | 6 | 15.2 | 232 | 4 |
| full | 10 | 40 | 20 | 40 | 0 | 2436 (2431–2440) | 0 | 0 | 100 | 60 | 1099 (1097–1101) | 2312 (2306–2315) | 40 |
| spec | 10 | 40 | 20 | 40 | 0 | 450 | 1988 (1981–1990) | 0 | 100 | 60 | 152 | 2314 (2306–2315) | 40 |
| full-shared | 10 | 4 | 2 | 40 | 0 | 2440 (2434–2440) | 0 | 40 | 100 | 60 | 1101 (1098–1101) | 232 (231–232) | 40 |
| spec-shared | 10 | 4 | 2 | 40 | 0 | 450 | 1990 (1989–1990) | 40 | 100 | 60 | 152 | 232 (231–232) | 40 |

### forced-disconnect

Writes performed (one run): `{"disconnect":6,"secret":18,"spec":24,"status":38}`

Client side (what the subscribers consumed):

| source | N | events added | modified | deleted | resets | notifications | renders | KiB down | reconnects | time-to-live ms (median) | time-to-live ms (max) | Secret values seen changing | redaction rev bumps |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 66 | 80 | 0 | 12 | 146 | 92 | 129 | 12 | 13.5 (12–14.5) | 19 (18–19) | 18 | 0 |
| full | 1 | 66 | 80 | 0 | 12 | 146 | 97 (96–97) | 66.7 | 12 | 11.5 (11.5–12) | 16 (15–18) | 0 | 18 |
| spec | 1 | 66 | 42 | 0 | 12 | 108 | 58 (55–61) | 44.3 | 12 | 11 (10–13) | 17 (17–20) | 0 | 18 |
| full-shared | 1 | 66 | 80 | 0 | 12 | 146 | 93 (92–95) | 66.7 | 12 | 12 (10.5–13.5) | 17 (16–21) | 0 | 18 |
| spec-shared | 1 | 66 | 42 | 0 | 12 | 108 | 54 (54–56) | 44.3 | 12 | 11.5 (10–12.5) | 19 (16–21) | 0 | 18 |
| native | 10 | 660 | 800 | 0 | 120 | 1460 | 920 | 1295 (1295–1295) | 120 | 30 (29–36.5) | 54 (41–54) | 180 | 0 |
| full | 10 | 660 | 800 | 0 | 120 | 1460 | 932 (928–961) | 667 (667–667) | 120 | 21 (19–24) | 51 (37–54) | 0 | 180 |
| spec | 10 | 660 | 420 | 0 | 120 | 1080 | 557 (555–574) | 443 (443–444) | 120 | 20 (19.5–24) | 51 (35–60) | 0 | 180 |
| full-shared | 10 | 660 | 800 | 0 | 120 | 1460 | 928 (927–928) | 667 (667–667) | 120 | 20 (17–23) | 48 (29–48) | 0 | 180 |
| spec-shared | 10 | 660 | 420 | 0 | 120 | 1080 | 547 (544–548) | 443 (443–444) | 120 | 20 (19–23) | 48 (32–59) | 0 | 180 |

Host side, native proxy:

| source | N | LIST | WATCH | WATCH after LIST | WATCH resumed | watch frames | ERROR frames | KiB to browser | KiB from API server | active watches at end | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|
| native | 1 | 12 | 12 | 12 | 0 | 80 | 0 | 129 | 129 | 2 | 24 |
| native | 10 | 120 | 120 | 120 | 0 | 800 | 0 | 1295 (1295–1295) | 1295 (1295–1295) | 20 | 240 |

Host side, gateway routes:

| source | N | upstream watches opened | active upstream at end | cycles | consumer resyncs | events emitted | suppressed | shared subscriptions | authorizer calls | of which timed | KiB SSE | KiB from API server | session checks |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| full | 1 | 12 | 2 | 12 | 0 | 170 | 0 | 0 | 12 | 0 | 66.7 | 133 (133–134) | 12 |
| spec | 1 | 12 | 2 | 12 | 0 | 132 | 38 | 0 | 12 | 0 | 44.3 | 133 (133–134) | 12 |
| full-shared | 1 | 12 | 2 | 12 | 0 | 170 | 0 | 12 | 12 | 0 | 66.7 | 133 (133–134) | 12 |
| spec-shared | 1 | 12 | 2 | 12 | 0 | 132 | 38 | 12 | 12 | 0 | 44.3 | 133 (133–134) | 12 |
| full | 10 | 120 | 20 | 120 | 0 | 1700 | 0 | 0 | 120 | 0 | 667 (667–667) | 1333 (1332–1334) | 120 |
| spec | 10 | 120 | 20 | 120 | 0 | 1320 | 380 | 0 | 120 | 0 | 443 (443–444) | 1333 (1332–1334) | 120 |
| full-shared | 10 | 12 | 2 | 120 | 0 | 1700 | 0 | 120 | 120 | 0 | 667 (667–667) | 133 (133–133) | 120 |
| spec-shared | 10 | 12 | 2 | 120 | 0 | 1320 | 380 | 120 | 120 | 0 | 443 (443–444) | 133 (133–133) | 120 |

## Reading the numbers

Everything here restates the tables; nothing is extrapolated beyond these objects, rates and
subscriber counts.

- **Events.** Native and full delivered the same number of events in every workload (churn, N = 1:
  214 `modified` each). Spec delivered 15: the 199 status-only writes were suppressed
  (`suppressed` = 199) and only the 9 spec writes and 6 Secret rotations arrived. Under burst, spec
  delivered 34 of 60 updates (the 26 status writes suppressed).
- **Bytes to the browser.** Churn, N = 10: native 2293 KiB, full 1099 KiB, spec 152 KiB. Quiet, where
  only the re-snapshots after the two forced reconnects travel: native 190 KiB, full 103 KiB, spec 93 KiB.
  The full and spec views remove `managedFields`, Secret values and (spec) status, which native carries.
- **Upstream watches and API-server bytes.** The unshared routes open one upstream watch per stream per
  snapshot cycle: 40 at N = 10 in quiet (10 subscribers × 2 collections × 2 reconnects). The shared
  routes opened 4 at both N = 1 and N = 10 — every subscriber left at the same forced disconnect, so the
  shared watch closed and reopened once per collection per reconnect. Under churn at N = 10 the
  unshared routes read 2312 KiB from the API server and the shared routes 232 KiB. The native proxy
  reads exactly what it forwards: 2293 KiB, with one LIST and one WATCH per collection per subscriber
  per reconnect.
- **Reconnects (BEFORE, native).** With this library every native reconnect re-LISTs: `LIST` =
  `WATCH` = `WATCH after LIST` and `WATCH resumed` = 0 in every workload, each followed by a `reset`
  and an `added` per object. The gateway connector also requests a fresh snapshot on every reconnect,
  so the gateway sources re-snapshot just as often; the difference is what each snapshot costs
  upstream and on the wire. Median time-to-live after a forced disconnect, with no backoff and all
  five sources reconnecting at once: 5–14 ms at N = 1 for every source; at N = 10, 28–46 ms for native
  and 16–34 ms for the gateway sources.
- **Authorization work.** The gateway's Authorizer runs per subscriber, shared or not: 100 calls at
  N = 10 in quiet, burst and churn on every route, 60 of them timed rechecks (5 s interval). In
  forced-disconnect no stream stayed live for 5 s, so there were no timed rechecks — only the 120
  cycle checks. The host's session check runs once per native request (2 per collection per
  reconnect) and once per gateway stream request (1).
- **Secret rotations.** Under churn each native subscriber saw the 6 rotated Secret values change; each
  full and spec subscriber saw 6 redaction revision bumps and never a value. Every projected store
  passed the "no Secret values" part of the correctness gate.

## Native AFTER: resumed watches

Measured 2026-10-05 14:29–14:46 UTC on the same machine and cluster, with

```bash
task compare-native-baseline BASELINE_REF=d259394
```

which built `d259394`'s library (re-list on every reconnect) in a scratch directory, measured the
native source with it, then with this branch's library (`packages/krm-stream` as in the commit
"feat(client): resume native watches from a consumed checkpoint"; the run's label `a391faa` is a
pre-rebase SHA of the same source), using identical defaults: 4 workloads × N 1 and 10 × 3
repetitions × 20 s, 2 forced reconnects per workload (12 in forced-disconnect), connector
`retryDelayMs` 0. Only the native source ran in these two passes, so time-to-live is not comparable
with the five-source tables above. All 48 runs passed the correctness gate: every store held each
object's exact resourceVersion, spec, status and Secret data after the workload, with resumed watches
and no re-snapshot. Cells are medians over 3 repetitions, summed over the N subscribers; every count
except time-to-live was identical across repetitions.

| workload | N | LIST before → after | WATCH resumed | resets | events added | notifications | KiB down (= KiB from API server) | time-to-live median ms |
|---|---|---|---|---|---|---|---|---|
| quiet | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 22 → 0 | 19 → 0 | 7.5 → 3.5 |
| quiet | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 220 → 0 | 190 → 0 | 34.5 → 8 |
| burst | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 82 → 60 | 78.8 → 59.8 | 8.5 → 2.5 |
| burst | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 820 → 600 | 789 → 598 | 22.5 → 6.5 |
| churn | 1 | 4 → 0 | 0 → 4 | 4 → 0 | 22 → 0 | 236 → 214 | 230 → 211 | 6 → 2.5 |
| churn | 10 | 40 → 0 | 0 → 40 | 40 → 0 | 220 → 0 | 2360 → 2140 | 2302 → 2113 | 16 → 5 |
| forced-disconnect | 1 | 12 → 0 | 0 → 12 | 12 → 0 | 66 → 0 | 146 → 80 | 129 → 72.3 | 8 → 3 |
| forced-disconnect | 10 | 120 → 0 | 0 → 120 | 120 → 0 | 660 → 0 | 1460 → 800 | 1295 → 723 | 14 → 5 |

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
task compare                      # the page: https://127.0.0.1:8111/
```

`examples/comparison/results/` keeps each run's JSON (every counter of every run) and markdown.
