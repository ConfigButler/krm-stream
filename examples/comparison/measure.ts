// The comparison's measurement driver. Node runs it directly (types are stripped), like the e2e
// drivers: the REAL connectors and stores, loaded from a configurable library build, against the
// comparison host (gateway/kube/examples/comparison) and its real API server.
//
// For every workload, subscriber count and repetition it opens N subscribers per source — each one a
// page watching the Widgets and the Secrets, one store per collection — waits until every one is
// live, triggers the workload through the host, waits for every store to converge on the cluster's
// state (the correctness gate: a run whose stores diverge is reported as failed, not measured),
// collects the host's counters and closes everything. It writes JSON and a markdown table.
//
//   node examples/comparison/measure.ts [options]
//   node examples/comparison/measure.ts --compare before.json after.json [--out file.md]
//
// Options (defaults in brackets):
//   --host URL             the comparison host [http://127.0.0.1:8110]
//   --lib DIR              built library directory [packages/krm-stream/dist]
//   --entry index|bundle   which entry point of it to load [index]
//   --sources LIST         native,full,spec,full-shared,spec-shared [all five]
//   --subscribers LIST     subscriber counts [1,10]
//   --workloads LIST       quiet,burst,churn,forced-disconnect [all four]
//   --reps N               repetitions [3]
//   --duration-ms N        workload duration [20000]
//   --seed N               workload seed [1]
//   --reconnects N         forced reconnects in every workload [2]
//   --retry-delay-ms N     connector retryDelayMs; 0 measures reconnect work without backoff [0]
//   --settle-ms N          how long stores may take to converge after a workload [20000]
//   --label TEXT           names this run in the output [current]
//   --out DIR              where results go [examples/comparison/results]
//   --session TOKEN        the viewer session [viewer-session]
//   --refused-session TOKEN  the refused session [refused-session]

import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

type Lib = typeof import("../../packages/krm-stream/src/index.ts");
type Store = InstanceType<Lib["LiveResourceStore"]>;
type Handle = ReturnType<Lib["connectNativeWatch"]>;
type StateEvent = Parameters<Parameters<Lib["connectNativeWatch"]>[1]>[0];

const repo = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const SOURCES = ["native", "full", "spec", "full-shared", "spec-shared"] as const;
type Source = (typeof SOURCES)[number];

// ------------------------------------------------------------------------------- arguments --

const argv = process.argv.slice(2);
function flag(name: string, fallback: string): string {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] !== undefined ? argv[i + 1]! : fallback;
}
const list = (value: string) => value.split(",").filter((v) => v !== "");

const opts = {
  host: flag("host", "http://127.0.0.1:8110").replace(/\/+$/, ""),
  lib: resolve(flag("lib", resolve(repo, "packages/krm-stream/dist"))),
  entry: flag("entry", "index"),
  sources: list(flag("sources", SOURCES.join(","))) as Source[],
  subscribers: list(flag("subscribers", "1,10")).map(Number),
  workloads: list(flag("workloads", "quiet,burst,churn,forced-disconnect")),
  reps: Number(flag("reps", "3")),
  durationMs: Number(flag("duration-ms", "20000")),
  seed: Number(flag("seed", "1")),
  reconnects: Number(flag("reconnects", "2")),
  retryDelayMs: Number(flag("retry-delay-ms", "0")),
  settleMs: Number(flag("settle-ms", "20000")),
  label: flag("label", "current"),
  out: resolve(flag("out", resolve(repo, "examples/comparison/results"))),
  session: flag("session", "viewer-session"),
  refusedSession: flag("refused-session", "refused-session"),
};
for (const s of opts.sources) if (!SOURCES.includes(s)) throw new Error(`unknown source ${s}`);

const libFile = resolve(opts.lib, opts.entry === "bundle" ? "krm-stream.js" : "index.js");
// Loaded by main(): a --compare invocation needs neither the library nor the host.
let lib: Lib;

// ------------------------------------------------------------------------------ the host --

interface PageConfig {
  namespace: string;
  group: string;
  widgets: string[];
  secrets: string[];
}
interface Metrics {
  elapsedMs: number;
  counters: Record<string, number>;
  gauges: Record<string, number>;
}
interface StateObject {
  kind: string;
  uid: string;
  name: string;
  resourceVersion: string;
  spec?: unknown;
  status?: unknown;
  data?: Record<string, string>;
}

async function host<T>(path: string, init: RequestInit = {}, session = opts.session): Promise<T> {
  const res = await fetch(`${opts.host}${path}`, {
    ...init,
    headers: { "X-Compare-Session": session, "Content-Type": "application/json", ...init.headers },
  });
  if (!res.ok) throw new Error(`${init.method ?? "GET"} ${path}: HTTP ${res.status} ${await res.text()}`);
  return (await res.json()) as T;
}

let config: PageConfig;

// ----------------------------------------------------------------------------- a subscriber --

/** What one collection feed saw. Counts are cumulative from the moment the feed opened. */
interface FeedCounts {
  events: Record<string, number>;
  notifications: number;
  renders: number;
  bytes: number;
  reconnects: number;
  errors: Record<string, number>;
  /** Native: a Secret's value changed. Full/spec: a redaction revision moved. */
  secretValueChanges: number;
  redactionRevBumps: number;
}

const emptyCounts = (): FeedCounts => ({
  events: {},
  notifications: 0,
  renders: 0,
  bytes: 0,
  reconnects: 0,
  errors: {},
  secretValueChanges: 0,
  redactionRevBumps: 0,
});

/** A fetch that counts the response bytes the connector reads. */
function countingFetch(onBytes: (n: number) => void): typeof fetch {
  return async (input, init) => {
    const res = await fetch(input, init);
    if (!res.body || [101, 204, 205, 304].includes(res.status)) return res;
    const counted = res.body.pipeThrough(
      new TransformStream<Uint8Array, Uint8Array>({
        transform(chunk, controller) {
          onBytes(chunk.byteLength);
          controller.enqueue(chunk);
        },
      }),
    );
    return new Response(counted, { status: res.status, statusText: res.statusText, headers: res.headers });
  };
}

/** One collection, one connection, one store. */
class Feed {
  readonly source: Source;
  readonly resource: "widgets" | "secrets";
  readonly store: Store;
  readonly handle: Handle;
  readonly counts = emptyCounts();
  /** Milliseconds from leaving `live` to being `live` again, once per recovery. */
  readonly recoveries: number[] = [];
  firstLiveMs: number | undefined;
  #lostAt: number | undefined;
  #renderPending = false;
  #secretValues = new Map<string, string>();
  #revs = new Map<string, number>();

  constructor(source: Source, resource: "widgets" | "secrets", session: string) {
    this.source = source;
    this.resource = resource;
    // A viewer: every region read-only. The store applies events exactly as an editing page's does.
    this.store = new lib.LiveResourceStore(lib.readOnlyPolicy);
    this.store.subscribe(() => {
      this.counts.notifications++;
      // A render is one per macrotask in which the store notified: the first notification after the
      // previous render schedules the next, as the next timer task.
      if (!this.#renderPending) {
        this.#renderPending = true;
        setTimeout(() => {
          this.#renderPending = false;
          this.counts.renders++;
        }, 0);
      }
    });
    const scope = {
      group: resource === "widgets" ? config.group : undefined,
      version: "v1",
      resource,
      namespace: config.namespace,
    };
    const options = {
      fetch: countingFetch((n) => {
        this.counts.bytes += n;
      }),
      headers: { "X-Compare-Session": session },
      retryDelayMs: opts.retryDelayMs,
      // Forced disconnects every few seconds would otherwise exhaust the default retry budget, which
      // resets only after 30 s live.
      healthyResetMs: 1000,
      onError: (code: string) => {
        this.counts.errors[code] = (this.counts.errors[code] ?? 0) + 1;
      },
    };
    const consume = (event: StateEvent) => {
      this.counts.events[event.type] = (this.counts.events[event.type] ?? 0) + 1;
      lib.applyStreamEvent(this.store, event);
      if (resource === "secrets" && (event.type === "added" || event.type === "modified")) this.#secret(event);
    };
    const started = performance.now();
    this.handle =
      source === "native"
        ? lib.connectNativeWatch(lib.nativeCollectionURL(`${opts.host}/k8s`, scope), consume, options)
        : lib.connectResourceStream(lib.resourceStreamURL(`${opts.host}/stream/${source}`, scope), consume, options);
    this.handle.subscribe((state) => {
      const now = performance.now();
      if (state.status === "live") {
        this.firstLiveMs ??= now - started;
        if (this.#lostAt !== undefined) this.recoveries.push(now - this.#lostAt);
        this.#lostAt = undefined;
      } else if (this.#lostAt === undefined && this.firstLiveMs !== undefined) {
        this.#lostAt = now;
      }
      if (state.status === "retrying") this.counts.reconnects++;
    });
    this.handle.closed.catch((error: unknown) => {
      this.counts.errors.HOST_EXCEPTION = (this.counts.errors.HOST_EXCEPTION ?? 0) + 1;
      console.error(`${source}/${resource}: consumer failed`, error);
    });
  }

  /** Track what a Secret event disclosed: a value (native) or a revision (projected). */
  #secret(event: Extract<StateEvent, { object: unknown }>) {
    const uid = event.object.metadata.uid;
    const value = JSON.stringify(event.object.data ?? null);
    const before = this.#secretValues.get(uid);
    if (before !== undefined && before !== value) this.counts.secretValueChanges++;
    this.#secretValues.set(uid, value);
    for (const r of event.redacted ?? []) {
      const key = `${uid} ${r.path}`;
      const prev = this.#revs.get(key);
      if (prev !== undefined && r.rev > prev) this.counts.redactionRevBumps++;
      this.#revs.set(key, r.rev);
    }
  }

  get live(): boolean {
    return this.handle.state.status === "live";
  }

  snapshot(): FeedCounts {
    return structuredClone(this.counts);
  }

  close(): Promise<void> {
    this.handle.close();
    return this.handle.closed.catch(() => {});
  }
}

// ------------------------------------------------------------------------- correctness gate --

/** Why a source's store does not hold what the cluster holds, or undefined when it does. */
function divergence(feed: Feed, state: StateObject[]): string | undefined {
  const kind = feed.resource === "widgets" ? "Widget" : "Secret";
  const want = state.filter((o) => o.kind === kind);
  const ids = feed.store.ids();
  if (ids.length !== want.length || !want.every((o) => ids.includes(o.uid))) {
    return `${kind} membership: store ${ids.length}, cluster ${want.length}`;
  }
  const projected = feed.source !== "native";
  const specOnly = feed.source.startsWith("spec");
  for (const o of want) {
    const got = feed.store.server(o.uid);
    if (kind === "Widget") {
      if (!lib.deepEqual(got.spec, o.spec)) return `${o.name}: spec differs`;
      if (specOnly ? got.status !== undefined : !lib.deepEqual(got.status, o.status)) {
        return `${o.name}: status ${specOnly ? "present in a spec projection" : "differs"}`;
      }
    } else if (projected) {
      // Never a value; exactly the cluster's keys, named as redacted paths.
      if (got.data !== undefined) return `${o.name}: a projected Secret carries values`;
      const paths = feed.store
        .redactions(o.uid)
        .map((r) => r.path.join("/"))
        .sort();
      const keys = Object.keys(o.data ?? {})
        .map((k) => `data/${k}`)
        .sort();
      if (!lib.deepEqual(paths, keys)) return `${o.name}: redacted paths ${paths} != ${keys}`;
    } else if (!lib.deepEqual(got.data, o.data)) {
      return `${o.name}: data differs`;
    }
    // A native store holds the cluster's exact version. A projection may legitimately hold an older
    // one: a suppressed update advances the cluster without an event.
    if (!projected && got.metadata.resourceVersion !== o.resourceVersion) return `${o.name}: resourceVersion differs`;
  }
  return undefined;
}

// -------------------------------------------------------------------------------- one run --

interface SourceResult {
  connectMs: number[];
  recoveriesMs: number[];
  /** Client counters summed over the source's subscribers, during the workload only. */
  client: FeedCounts;
  /** Host counters for this source, during the workload only. */
  host: Record<string, number>;
  /** Host counters for this source during the initial connect. */
  hostConnect: Record<string, number>;
  /** Gauges at the end of the workload, before closing. */
  gauges: Record<string, number>;
  divergence?: string;
}

interface RunResult {
  workload: string;
  subscribers: number;
  rep: number;
  ok: boolean;
  failure?: string;
  settleMs: number;
  workloadResult: unknown;
  sessions: Record<string, number>;
  sources: Partial<Record<Source, SourceResult>>;
}

function sumCounts(all: FeedCounts[]): FeedCounts {
  const out = emptyCounts();
  for (const c of all) {
    for (const [k, v] of Object.entries(c.events)) out.events[k] = (out.events[k] ?? 0) + v;
    for (const [k, v] of Object.entries(c.errors)) out.errors[k] = (out.errors[k] ?? 0) + v;
    out.notifications += c.notifications;
    out.renders += c.renders;
    out.bytes += c.bytes;
    out.reconnects += c.reconnects;
    out.secretValueChanges += c.secretValueChanges;
    out.redactionRevBumps += c.redactionRevBumps;
  }
  return out;
}

function minus(after: FeedCounts, before: FeedCounts): FeedCounts {
  const out = emptyCounts();
  for (const k of new Set([...Object.keys(after.events), ...Object.keys(before.events)]))
    out.events[k] = (after.events[k] ?? 0) - (before.events[k] ?? 0);
  for (const k of new Set([...Object.keys(after.errors), ...Object.keys(before.errors)]))
    out.errors[k] = (after.errors[k] ?? 0) - (before.errors[k] ?? 0);
  out.notifications = after.notifications - before.notifications;
  out.renders = after.renders - before.renders;
  out.bytes = after.bytes - before.bytes;
  out.reconnects = after.reconnects - before.reconnects;
  out.secretValueChanges = after.secretValueChanges - before.secretValueChanges;
  out.redactionRevBumps = after.redactionRevBumps - before.redactionRevBumps;
  return out;
}

/** The host counters that belong to one source. */
function hostFor(source: Source, counters: Record<string, number>): Record<string, number> {
  const prefix = source === "native" ? "native." : `gateway.${source}.`;
  return Object.fromEntries(
    Object.entries(counters)
      .filter(([k]) => k.startsWith(prefix))
      .map(([k, v]) => [k.slice(prefix.length), v]),
  );
}

function diff(after: Record<string, number>, before: Record<string, number>): Record<string, number> {
  const out: Record<string, number> = {};
  for (const k of new Set([...Object.keys(after), ...Object.keys(before)])) {
    const d = (after[k] ?? 0) - (before[k] ?? 0);
    if (d !== 0) out[k] = d;
  }
  return out;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function waitFor(what: string, timeoutMs: number, done: () => boolean | Promise<boolean>) {
  const deadline = performance.now() + timeoutMs;
  while (!(await done())) {
    if (performance.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(50);
  }
}

async function runOnce(workload: string, n: number, rep: number): Promise<RunResult> {
  await host("/admin/reset", { method: "POST" });
  await host("/metrics/reset", { method: "POST" });
  const feeds = new Map<Source, Feed[]>();
  for (const source of opts.sources) {
    const list: Feed[] = [];
    for (let i = 0; i < n; i++) {
      list.push(new Feed(source, "widgets", opts.session), new Feed(source, "secrets", opts.session));
    }
    feeds.set(source, list);
  }
  const all = [...feeds.values()].flat();
  const result: RunResult = {
    workload,
    subscribers: n,
    rep,
    ok: false,
    settleMs: 0,
    workloadResult: undefined,
    sessions: {},
    sources: {},
  };
  try {
    await waitFor("every subscriber live", 60_000, () => all.every((f) => f.live));
    await sleep(250); // let the last snapshot's renders run before the baseline is taken
    const m0 = await host<Metrics>("/metrics");
    const before = new Map(all.map((f) => [f, f.snapshot()]));

    result.workloadResult = await host("/admin/workload", {
      method: "POST",
      body: JSON.stringify({
        name: workload,
        durationMs: opts.durationMs,
        seed: opts.seed,
        reconnects: opts.reconnects,
      }),
    });

    // The correctness gate: every store converges on the cluster, or the run failed.
    const settleStart = performance.now();
    let state: StateObject[] = [];
    let last: string | undefined;
    try {
      await waitFor("convergence", opts.settleMs, async () => {
        state = await host<StateObject[]>("/admin/state");
        last = undefined;
        for (const f of all) {
          if (!f.live) {
            last = `${f.source}/${f.resource} not live (${f.handle.state.status})`;
            return false;
          }
          const why = divergence(f, state);
          if (why) {
            last = `${f.source}/${f.resource}: ${why}`;
            return false;
          }
        }
        return true;
      });
      result.ok = true;
    } catch {
      result.failure = `stores did not converge within ${opts.settleMs} ms: ${last}`;
    }
    result.settleMs = Math.round(performance.now() - settleStart);
    await sleep(250);
    const m1 = await host<Metrics>("/metrics");
    result.sessions = Object.fromEntries(Object.entries(m1.counters).filter(([k]) => k.startsWith("sessions.")));
    for (const [source, list] of feeds) {
      const client = sumCounts(list.map((f) => minus(f.snapshot(), before.get(f)!)));
      result.sources[source] = {
        connectMs: list.map((f) => Math.round(f.firstLiveMs ?? -1)),
        recoveriesMs: list.flatMap((f) => f.recoveries.map(Math.round)),
        client,
        host: diff(hostFor(source, m1.counters), hostFor(source, m0.counters)),
        hostConnect: hostFor(source, m0.counters),
        gauges: hostFor(source, m1.gauges),
        divergence: list.map((f) => divergence(f, state)).find((d) => d !== undefined),
      };
    }
  } finally {
    await Promise.all(all.map((f) => f.close()));
  }
  return result;
}

// ---------------------------------------------------------- refusal is never a native fallback --

/** Opens every gateway route as the refused session and checks that each ends terminally, and that
 * nothing reached the native proxy for that session afterwards. This driver never falls back; the
 * host's counter is the independent witness. */
async function refusalCheck() {
  const before = await host<Metrics>("/metrics");
  const routes = ["full", "spec", "full-shared", "spec-shared"];
  const outcomes: Record<string, string> = {};
  await Promise.all(
    routes.map(async (route) => {
      let error = "";
      const handle = lib.connectResourceStream(
        lib.resourceStreamURL(`${opts.host}/stream/${route}`, {
          group: config.group,
          version: "v1",
          resource: "widgets",
          namespace: config.namespace,
        }),
        () => {},
        {
          headers: { "X-Compare-Session": opts.refusedSession },
          onError: (code, _message, terminal) => {
            error = `${code}${terminal ? " (terminal)" : ""}`;
          },
        },
      );
      await handle.closed.catch(() => {});
      outcomes[route] = `${error}, final state ${handle.state.status}`;
    }),
  );
  const after = await host<Metrics>("/metrics");
  const nativeAfter =
    (after.counters["native.refused_session_requests"] ?? 0) -
    (before.counters["native.refused_session_requests"] ?? 0);
  const ok =
    nativeAfter === 0 &&
    Object.values(outcomes).every((o) => o.startsWith("FORBIDDEN (terminal), final state terminal"));
  return { ok, outcomes, nativeRequestsByRefusedSessionAfterRefusal: nativeAfter };
}

// ------------------------------------------------------------------------------- reporting --

const median = (xs: number[]) => {
  if (xs.length === 0) return Number.NaN;
  const s = [...xs].sort((a, b) => a - b);
  const mid = Math.floor(s.length / 2);
  return s.length % 2 ? s[mid]! : (s[mid - 1]! + s[mid]!) / 2;
};

function fmt(xs: number[]): string {
  if (xs.length === 0 || xs.every((x) => Number.isNaN(x))) return "–";
  const ys = xs.filter((x) => !Number.isNaN(x));
  const round = (x: number) => (Math.abs(x) >= 100 ? Math.round(x).toString() : (Math.round(x * 10) / 10).toString());
  const lo = Math.min(...ys);
  const hi = Math.max(...ys);
  return lo === hi ? round(lo) : `${round(median(ys))} (${round(lo)}–${round(hi)})`;
}

const kb = (x: number) => x / 1024;

/** The metrics each table reports, per source run. */
const clientColumns: [string, (s: SourceResult) => number][] = [
  ["events added", (s) => s.client.events.added ?? 0],
  ["modified", (s) => s.client.events.modified ?? 0],
  ["deleted", (s) => s.client.events.deleted ?? 0],
  ["resets", (s) => s.client.events.reset ?? 0],
  ["notifications", (s) => s.client.notifications],
  ["renders", (s) => s.client.renders],
  ["KiB down", (s) => kb(s.client.bytes)],
  ["reconnects", (s) => s.client.reconnects],
  ["time-to-live ms (median)", (s) => median(s.recoveriesMs)],
  ["time-to-live ms (max)", (s) => (s.recoveriesMs.length ? Math.max(...s.recoveriesMs) : Number.NaN)],
  ["Secret values seen changing", (s) => s.client.secretValueChanges],
  ["redaction rev bumps", (s) => s.client.redactionRevBumps],
];

const hostColumns: Record<"native" | "gateway", [string, (s: SourceResult) => number][]> = {
  native: [
    ["LIST", (s) => s.host.list_requests ?? 0],
    ["WATCH", (s) => s.host.watch_requests ?? 0],
    ["WATCH after LIST", (s) => s.host.watch_after_list ?? 0],
    ["WATCH resumed", (s) => s.host.watch_resumed ?? 0],
    ["watch frames", (s) => sumPrefix(s.host, "watch_frames.")],
    ["ERROR frames", (s) => s.host["watch_frames.ERROR"] ?? 0],
    ["KiB to browser", (s) => kb(sumPrefix(s.host, "bytes."))],
    ["KiB from API server", (s) => kb(s.host.upstream_bytes ?? 0)],
    ["active watches at end", (s) => s.gauges.watches_active ?? 0],
    ["session checks", (s) => s.host.session_checks ?? 0],
  ],
  gateway: [
    ["upstream watches opened", (s) => s.host.upstream_watch_calls ?? 0],
    ["active upstream at end", (s) => s.gauges.upstream_watches_active ?? 0],
    ["cycles", (s) => s.host["obs.cycle_started"] ?? 0],
    ["consumer resyncs", (s) => s.host["obs.consumer_resync"] ?? 0],
    ["events emitted", (s) => sumPrefix(s.host, "obs.event_emitted.")],
    ["suppressed", (s) => sumPrefix(s.host, "obs.event_suppressed.")],
    ["shared subscriptions", (s) => s.host["obs.shared_subscription_opened"] ?? 0],
    ["authorizer calls", (s) => (s.host["authorizer_calls.cycle"] ?? 0) + (s.host["authorizer_calls.timed"] ?? 0)],
    ["of which timed", (s) => s.host["authorizer_calls.timed"] ?? 0],
    ["KiB SSE", (s) => kb(s.host.sse_bytes ?? 0)],
    ["KiB from API server", (s) => kb(s.host.upstream_bytes ?? 0)],
    ["session checks", (s) => s.host.session_checks ?? 0],
  ],
};

function sumPrefix(m: Record<string, number>, prefix: string): number {
  return Object.entries(m)
    .filter(([k]) => k.startsWith(prefix))
    .reduce((a, [, v]) => a + v, 0);
}

function table(
  runs: RunResult[],
  workload: string,
  sources: Source[],
  columns: [string, (s: SourceResult) => number][],
): string {
  const lines = [
    `| source | N | ${columns.map(([c]) => c).join(" | ")} |`,
    `|---|---|${columns.map(() => "---|").join("")}`,
  ];
  for (const n of [...new Set(runs.map((r) => r.subscribers))]) {
    for (const source of sources) {
      const mine = runs.filter((r) => r.workload === workload && r.subscribers === n && r.ok && r.sources[source]);
      if (mine.length === 0) continue;
      const cells = columns.map(([, get]) => fmt(mine.map((r) => get(r.sources[source]!))));
      lines.push(`| ${source} | ${n} | ${cells.join(" | ")} |`);
    }
  }
  return lines.join("\n");
}

function markdown(results: Results): string {
  const runs = results.runs;
  const out: string[] = [
    `# Comparison run: ${results.label}`,
    "",
    `Library: \`${results.library}\` · host: ${results.host} · ${results.startedAt} → ${results.finishedAt}`,
    "",
    `Workloads ${opts.workloads.join(", ")}; subscribers ${opts.subscribers.join(", ")}; ${opts.reps} repetitions; ` +
      `duration ${opts.durationMs} ms; seed ${opts.seed}; ${opts.reconnects} forced reconnects per workload; ` +
      `retryDelayMs ${opts.retryDelayMs}. Cells are median (min–max) over the repetitions that passed the ` +
      "correctness gate; client and host counts cover the workload phase only, summed over the N subscribers " +
      "(each subscriber watches the Widgets and the Secrets, one connection and one store each).",
    "",
    `Refusal check: ${results.refusal.ok ? "passed" : "FAILED"} — ${JSON.stringify(results.refusal.outcomes)}; ` +
      `native requests by the refused session afterwards: ${results.refusal.nativeRequestsByRefusedSessionAfterRefusal}.`,
    "",
  ];
  const failed = runs.filter((r) => !r.ok);
  out.push(
    failed.length === 0
      ? `All ${runs.length} runs passed the correctness gate.`
      : `**${failed.length} of ${runs.length} runs FAILED the correctness gate and are excluded:** ` +
          failed.map((r) => `${r.workload}/N=${r.subscribers}/rep ${r.rep}: ${r.failure}`).join("; "),
    "",
  );
  const gatewaySources = opts.sources.filter((s) => s !== "native");
  for (const workload of opts.workloads) {
    const w = runs.find((r) => r.workload === workload)?.workloadResult as { done?: object } | undefined;
    out.push(`## ${workload}`, "", `Writes performed (one run): \`${JSON.stringify(w?.done ?? {})}\``, "");
    out.push(
      "Client side (what the subscribers consumed):",
      "",
      table(runs, workload, opts.sources, clientColumns),
      "",
    );
    if (opts.sources.includes("native")) {
      out.push("Host side, native proxy:", "", table(runs, workload, ["native"], hostColumns.native), "");
    }
    if (gatewaySources.length) {
      out.push("Host side, gateway routes:", "", table(runs, workload, gatewaySources, hostColumns.gateway), "");
    }
  }
  return out.join("\n");
}

interface Results {
  label: string;
  library: string;
  host: string;
  startedAt: string;
  finishedAt: string;
  options: typeof opts;
  refusal: Awaited<ReturnType<typeof refusalCheck>>;
  runs: RunResult[];
}

/** A native before/after table from two result files. */
function compareFiles(beforePath: string | undefined, afterPath: string | undefined, out: string) {
  if (!beforePath || !afterPath) throw new Error("usage: measure.ts --compare before.json after.json [--out file.md]");
  const before = JSON.parse(readFileSync(beforePath, "utf8")) as Results;
  const after = JSON.parse(readFileSync(afterPath, "utf8")) as Results;
  const columns = [...clientColumns, ...hostColumns.native];
  const lines = [
    `# Native before/after: ${before.label} → ${after.label}`,
    "",
    `Before: \`${before.library}\`. After: \`${after.library}\`. Cells are median (min–max) over passing repetitions.`,
    "",
  ];
  for (const workload of before.options.workloads) {
    lines.push(`## ${workload}`, "", `| metric | N | ${before.label} | ${after.label} |`, "|---|---|---|---|");
    for (const n of before.options.subscribers) {
      for (const [name, get] of columns) {
        const cell = (r: Results) =>
          fmt(
            r.runs
              .filter((x) => x.workload === workload && x.subscribers === n && x.ok && x.sources.native)
              .map((x) => get(x.sources.native!)),
          );
        lines.push(`| ${name} | ${n} | ${cell(before)} | ${cell(after)} |`);
      }
    }
    lines.push("");
  }
  const failed = [before, after].flatMap((r) => r.runs.filter((x) => !x.ok).map((x) => `${r.label}: ${x.failure}`));
  if (failed.length) lines.push(`**Failed runs (excluded):** ${failed.join("; ")}`, "");
  const text = lines.join("\n");
  if (out) writeFileSync(out, text);
  process.stdout.write(`${text}\n`);
}

// ------------------------------------------------------------------------------------ main --

if (argv[0] === "--compare") {
  compareFiles(argv[1], argv[2], flag("out", ""));
} else {
  await main();
}

async function main() {
  lib = (await import(pathToFileURL(libFile).href)) as Lib;
  config = await host<PageConfig>("/config");
  const startedAt = new Date().toISOString();
  console.error(`library ${libFile}; namespace ${config.namespace}`);
  const refusal = await refusalCheck();
  console.error(`refusal check: ${refusal.ok ? "passed" : "FAILED"} ${JSON.stringify(refusal)}`);
  const runs: RunResult[] = [];
  for (const workload of opts.workloads) {
    for (const n of opts.subscribers) {
      for (let rep = 1; rep <= opts.reps; rep++) {
        const t0 = performance.now();
        const run = await runOnce(workload, n, rep);
        runs.push(run);
        console.error(
          `${workload} N=${n} rep ${rep}: ${run.ok ? "ok" : `FAILED ${run.failure}`} ` +
            `(${Math.round((performance.now() - t0) / 1000)} s, settled in ${run.settleMs} ms)`,
        );
      }
    }
  }
  const results: Results = {
    label: opts.label,
    library: libFile,
    host: opts.host,
    startedAt,
    finishedAt: new Date().toISOString(),
    options: opts,
    refusal,
    runs,
  };
  mkdirSync(opts.out, { recursive: true });
  const stem = resolve(opts.out, `${startedAt.replace(/[:.]/g, "-")}-${opts.label}`);
  writeFileSync(`${stem}.json`, `${JSON.stringify(results, null, 2)}\n`);
  writeFileSync(`${stem}.md`, `${markdown(results)}\n`);
  console.error(`wrote ${stem}.json and ${stem}.md`);
  process.stdout.write(`${stem}.json\n`);
  process.exit(refusal.ok && runs.every((r) => r.ok) ? 0 : 1);
}
