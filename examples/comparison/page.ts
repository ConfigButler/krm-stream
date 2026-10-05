// The comparison page's controller: the same Widgets and Secrets through three sources side by side
// — native, krm-full/v1 and krm-spec/v1 — with editing on every one of them, using the existing
// connectors, stores, editors and recipes. build.mjs compiles it into dist/page.js.
//
// One store per source, collection and identity. A source's store is never fed by another source,
// and the page never opens a native connection for a session the gateway refused.

import {
  applyStreamEvent,
  type ConnectionState,
  connectNativeWatch,
  connectResourceStream,
  type KRMObject,
  LiveResourceStore,
  nativeCollectionURL,
  type Path,
  type ResourceStateEvent,
  type ResourceStreamHandle,
  resourceStreamURL,
} from "../../packages/krm-stream/src/index.ts";
import { conditionalEditor } from "../conditional-save/editor.ts";
import { keepLocal } from "../editor-recipes/keepLocal.ts";
import { nativeEditor } from "../native-editor/editor.ts";
import { draftText, editField, showDraft } from "./fields.ts";

interface PageConfig {
  namespace: string;
  group: string;
  widgets: string[];
  secrets: string[];
  refusedSession: string;
  workloads: string[];
}

type Kind = "native" | "full" | "spec";
type Editor = { readonly saving: boolean; save(): Promise<string> };

const $ = <T extends HTMLElement = HTMLElement>(selector: string, root: ParentNode = document) =>
  root.querySelector(selector) as T;
const el = <K extends keyof HTMLElementTagNameMap>(tag: K, text = "", attrs: Record<string, string> = {}) => {
  const node = document.createElement(tag);
  node.textContent = text;
  for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
  return node;
};

const params = new URLSearchParams(location.search);
const config = (await (await fetch("/config", { cache: "no-store" })).json()) as PageConfig;
$("#target").textContent =
  `namespace ${config.namespace} · entry ${params.get("entry") === "bundle" ? "dist/krm-stream.js" : "dist/index.js"}`;
// This page holds six streams. Over HTTP/1.1 a browser allows six connections per origin, so every
// save and metrics poll would wait behind them; the host's TLS listener gives the browser HTTP/2.
if (location.protocol === "http:") {
  $("#protocol").textContent =
    "Served over plain HTTP/1.1: this page's six streams use every connection the browser allows this " +
    "origin, so saves will wait. Open the host's https:// address (HTTP/2) instead.";
}

const widgetScope = { group: config.group, version: "v1", resource: "widgets", namespace: config.namespace };
const secretScope = { version: "v1", resource: "secrets", namespace: config.namespace };
// The host's fetch: same-origin, so the session cookie goes with every request. Add CSRF here.
const request: typeof fetch = (input, init) => fetch(input, init);

/** A fetch that counts the response bytes a connector reads. */
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

interface Feed {
  store: LiveResourceStore;
  handle: ResourceStreamHandle;
}

/** One source: its two collections, its counters and its editor. */
class Column {
  readonly kind: Kind;
  readonly root: HTMLElement;
  route = "";
  widgets?: Feed;
  secrets?: Feed;
  counts = { events: {} as Record<string, number>, notifications: 0, renders: 0, bytes: 0, reconnects: 0 };
  error = "";
  editor?: Editor;
  editorUid?: string;
  #renderQueued = false;

  constructor(kind: Kind, root: HTMLElement) {
    this.kind = kind;
    this.root = root;
  }

  /** (Re)open both collections on a route with NEW stores: a store is never reused across routes. */
  open(route: string) {
    this.close();
    this.route = route;
    this.editor = undefined;
    this.editorUid = undefined;
    this.counts = { events: {}, notifications: 0, renders: 0, bytes: 0, reconnects: 0 };
    this.error = "";
    this.widgets = this.#feed(widgetScope);
    this.secrets = this.#feed(secretScope);
    $("[data-role=route]", this.root).textContent = this.kind === "native" ? "/k8s (host proxy)" : `/stream/${route}`;
    this.render();
  }

  #feed(scope: typeof widgetScope | typeof secretScope): Feed {
    const store = new LiveResourceStore();
    store.subscribe(() => {
      this.counts.notifications++;
      this.render();
    });
    const consume = (event: ResourceStateEvent) => {
      this.counts.events[event.type] = (this.counts.events[event.type] ?? 0) + 1;
      applyStreamEvent(store, event);
    };
    const options = {
      fetch: countingFetch((n) => {
        this.counts.bytes += n;
        this.render();
      }),
      onError: (code: string, message: string, terminal: boolean) => {
        this.error = `${code}${terminal ? " (terminal)" : ""}: ${message}`;
        this.render();
      },
    };
    const handle =
      this.kind === "native"
        ? connectNativeWatch(nativeCollectionURL("/k8s", scope), consume, options)
        : connectResourceStream(resourceStreamURL(`/stream/${this.route}`, scope), consume, options);
    handle.subscribe((state: Readonly<ConnectionState>) => {
      if (state.status === "retrying") this.counts.reconnects++;
      this.render();
    });
    handle.closed.catch((error: unknown) => {
      this.error = `page error: ${error}`;
      this.render();
    });
    return { store, handle };
  }

  close() {
    this.widgets?.handle.close();
    this.secrets?.handle.close();
  }

  live(): boolean {
    return this.widgets?.handle.state.status === "live";
  }

  /** The widget being edited in this column, by the name selected for every column. */
  selected(): string | undefined {
    const store = this.widgets?.store;
    const name = $<HTMLSelectElement>("#widget").value;
    return store?.ids().find((id) => store.server(id).metadata.name === name);
  }

  editorFor(uid: string): Editor {
    if (this.editor && this.editorUid === uid) return this.editor;
    const store = this.widgets!.store;
    const live = () => this.live();
    const name = store.server(uid).metadata.name;
    this.editor =
      this.kind === "native"
        ? nativeEditor(store, uid, { proxy: "/k8s", scope: widgetScope }, request, live)
        : conditionalEditor(store, uid, `/save/${this.kind}/widgets/${name}`, request, live);
    this.editorUid = uid;
    return this.editor;
  }

  async save() {
    const uid = this.selected();
    if (!uid) return;
    const outcome = $("[data-role=outcome]", this.root);
    outcome.textContent = "saving…";
    this.render();
    try {
      const result = await this.editorFor(uid).save();
      outcome.textContent = result;
    } catch (error) {
      outcome.textContent = `error: ${error instanceof Error ? error.message : String(error)}`;
    }
    outcome.dataset.at = String(Date.now());
    this.render();
  }

  /** One render per animation frame in which anything changed. */
  render() {
    if (this.#renderQueued) return;
    this.#renderQueued = true;
    requestAnimationFrame(() => {
      this.#renderQueued = false;
      this.counts.renders++;
      this.#draw();
    });
  }

  #draw() {
    const r = this.root;
    const w = this.widgets?.handle.state.status ?? "closed";
    const s = this.secrets?.handle.state.status ?? "closed";
    $("[data-role=state]", r).textContent = w === s ? w : `widgets ${w} · secrets ${s}`;
    $("[data-role=state]", r).dataset.status = w === "live" && s === "live" ? "live" : w;
    $("[data-role=error]", r).textContent = this.error;
    const ev = this.counts.events;
    $("[data-role=counters]", r).textContent =
      `events reset ${ev.reset ?? 0} · added ${ev.added ?? 0} · modified ${ev.modified ?? 0} · deleted ${ev.deleted ?? 0}` +
      ` · notifications ${this.counts.notifications} · renders ${this.counts.renders}` +
      ` · ${(this.counts.bytes / 1024).toFixed(1)} KiB · reconnects ${this.counts.reconnects}`;
    if (!this.widgets || !this.secrets) return;
    this.#drawWidgets(this.widgets.store);
    this.#drawEditor(this.widgets.store);
    this.#drawSecrets(this.secrets.store);
  }

  #drawWidgets(store: LiveResourceStore) {
    const rows = $("[data-role=rows]", this.root);
    rows.replaceChildren(
      ...store
        .ids()
        .map((id) => store.server(id))
        .sort((a, b) => a.metadata.name.localeCompare(b.metadata.name))
        .map((o: KRMObject) => {
          const spec = (o.spec ?? {}) as Record<string, unknown>;
          const status = o.status as Record<string, unknown> | undefined;
          const tr = el("tr", "", { "data-name": o.metadata.name });
          tr.append(
            el("td", o.metadata.name),
            el("td", o.metadata.resourceVersion ?? ""),
            el("td", String(spec.replicas ?? "")),
            el("td", String(spec.revision ?? "")),
            el("td", String(spec.note ?? ""), { "data-role": "server-note" }),
            el("td", status ? `${status.phase} #${status.heartbeat}` : "— (not in view)"),
          );
          return tr;
        }),
    );
  }

  #drawEditor(store: LiveResourceStore) {
    const r = this.root;
    const uid = this.selected();
    const fields = $<HTMLFieldSetElement>("[data-role=editor]", r);
    fields.disabled = uid === undefined;
    if (!uid) return;
    const editor = this.editorFor(uid);
    for (const [field, input] of [
      ["note", $<HTMLInputElement>("[data-role=note]", r)],
      ["replicas", $<HTMLInputElement>("[data-role=replicas]", r)],
    ] as const) {
      const path: Path = ["spec", field];
      // Follow the draft even while focused: a stale input would save its stale text. See fields.ts.
      showDraft(input, uid, draftText(store, uid, path), document.activeElement === input);
      input.classList.toggle("dirty", store.isDirty(uid, path));
      input.classList.toggle(
        "conflicted",
        store.conflicts(uid).some((c) => c.path.join("/") === path.join("/")),
      );
    }
    const save = $<HTMLButtonElement>("[data-role=save]", r);
    save.disabled = !this.live() || editor.saving;
    $("[data-role=dirty]", r).textContent = store
      .changes(uid)
      .map((c) => `${c.path.join(".")}: ${JSON.stringify(c.old)} → ${JSON.stringify(c.new)}`)
      .join("; ");
    const conflicts = $("[data-role=conflicts]", r);
    conflicts.replaceChildren(
      ...store.conflicts(uid).map((c) => {
        const li = el("li", `${c.path.join(".")}: server has ${JSON.stringify(c.theirs)} `, {
          "data-path": c.path.join("."),
        });
        const theirs = el("button", "Take theirs", { type: "button", "data-role": "take-theirs" });
        theirs.onclick = () => store.revert(uid, c.path);
        const mine = el("button", "Keep mine", { type: "button", "data-role": "keep-mine" });
        mine.onclick = () => keepLocal(store, uid, c.path);
        li.append(theirs, mine);
        return li;
      }),
    );
  }

  #drawSecrets(store: LiveResourceStore) {
    const list = $("[data-role=secrets]", this.root);
    list.replaceChildren(
      ...store
        .ids()
        .map((id) => ({ id, o: store.server(id) }))
        .sort((a, b) => a.o.metadata.name.localeCompare(b.o.metadata.name))
        .map(({ id, o }) => {
          const li = el("li", "", { "data-name": o.metadata.name });
          const data = o.data as Record<string, string> | undefined;
          if (data) {
            // Native: the values themselves, decoded. This is what native access discloses.
            li.textContent = `${o.metadata.name}: ${Object.entries(data)
              .map(([k, v]) => `${k}=${safeDecode(v)}`)
              .join(", ")}`;
            li.dataset.disclosure = "values";
          } else {
            // Full/spec: which paths exist and how often they changed. Never a value.
            li.textContent = `${o.metadata.name}: ${store
              .redactions(id)
              .map((x) => `/${x.path.join("/")} ••• rev ${x.rev}`)
              .join(", ")}`;
            li.dataset.disclosure = "redacted";
          }
          return li;
        }),
    );
  }
}

function safeDecode(value: string): string {
  try {
    return atob(value);
  } catch {
    return value;
  }
}

// ------------------------------------------------------------------------------ the columns --

const columns = (["native", "full", "spec"] as const).map((kind) => {
  const root = $<HTMLElement>(`[data-source=${kind}]`);
  root.append(($<HTMLTemplateElement>("#column").content.cloneNode(true) as DocumentFragment).firstElementChild!);
  $("[data-role=title]", root).textContent =
    kind === "native" ? "native" : kind === "full" ? "gateway krm-full/v1" : "gateway krm-spec/v1";
  const column = new Column(kind, root);
  $("[data-role=save]", root).addEventListener("click", () => void column.save());
  for (const field of ["note", "replicas"] as const) {
    $<HTMLInputElement>(`[data-role=${field}]`, root).addEventListener("input", (e) => {
      const uid = column.selected();
      if (!uid || !column.widgets) return;
      const input = e.target as HTMLInputElement;
      editField(column.widgets.store, uid, ["spec", field], input, field === "replicas" ? Number : undefined);
    });
  }
  return column;
});

const select = $<HTMLSelectElement>("#widget");
select.replaceChildren(...config.widgets.map((name) => el("option", name, { value: name })));
select.addEventListener("change", () => {
  for (const c of columns) c.render();
});

const variant = $<HTMLSelectElement>("#variant");
variant.value = params.get("variant") === "shared" ? "shared" : "unshared";
function openAll() {
  const suffix = variant.value === "shared" ? "-shared" : "";
  for (const c of columns) c.open(c.kind === "native" ? "native" : `${c.kind}${suffix}`);
}
variant.addEventListener("change", openAll);
openAll();

// --------------------------------------------------------------------------- host controls --

async function admin(path: string, body?: unknown): Promise<unknown> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${path}: HTTP ${res.status} ${text}`);
  return JSON.parse(text);
}

const report = (text: string) => {
  $("#admin-result").textContent = text;
};
const workload = $<HTMLSelectElement>("#workload");
workload.replaceChildren(...config.workloads.map((name) => el("option", name, { value: name })));
$("#run").addEventListener("click", async () => {
  report(`running ${workload.value}…`);
  try {
    const durationMs = Number($<HTMLInputElement>("#duration").value);
    report(JSON.stringify(await admin("/admin/workload", { name: workload.value, durationMs })));
  } catch (error) {
    report(String(error));
  }
});
for (const target of ["native", "gateway", "all"]) {
  $(`#disconnect-${target}`).addEventListener("click", async () => {
    report(JSON.stringify(await admin(`/admin/disconnect?target=${target}`)));
  });
}
$("#competing").addEventListener("click", async () => {
  const note = `server-${Date.now() % 100000}`;
  await admin("/admin/write", { name: select.value, note });
  report(`another writer set ${select.value} spec.note = ${note}`);
});
$("#reset-counters").addEventListener("click", async () => {
  await admin("/metrics/reset");
  for (const c of columns) {
    c.counts = { events: {}, notifications: 0, renders: 0, bytes: 0, reconnects: 0 };
    c.render();
  }
});

// ----------------------------------------------------------------------------- host metrics --

const metricRows: [string, string][] = [
  ["native", "native."],
  ["full", "gateway.full."],
  ["spec", "gateway.spec."],
  ["full-shared", "gateway.full-shared."],
  ["spec-shared", "gateway.spec-shared."],
];
const metricColumns: [string, (c: Record<string, number>, g: Record<string, number>) => number][] = [
  ["LIST / upstream watches", (c) => c.list_requests ?? c.upstream_watch_calls ?? 0],
  ["WATCH (resumed)", (c) => c.watch_requests ?? Number.NaN],
  ["active upstream", (_c, g) => g.watches_active ?? g.upstream_watches_active ?? 0],
  ["events sent", (c) => sum(c, "watch_frames.") + sum(c, "obs.event_emitted.")],
  ["suppressed", (c) => (c.list_requests === undefined ? sum(c, "obs.event_suppressed.") : Number.NaN)],
  ["KiB to browsers", (c) => Math.round((sum(c, "bytes.") + (c.sse_bytes ?? 0)) / 1024)],
  ["authorizer calls", (c) => (c.list_requests === undefined ? sum(c, "authorizer_calls.") : Number.NaN)],
  ["session checks", (c) => c.session_checks ?? 0],
];
function sum(m: Record<string, number>, prefix: string) {
  return Object.entries(m)
    .filter(([k]) => k.startsWith(prefix))
    .reduce((a, [, v]) => a + v, 0);
}
async function pollMetrics() {
  try {
    const res = await fetch("/metrics", { cache: "no-store" });
    const { counters, gauges } = (await res.json()) as {
      counters: Record<string, number>;
      gauges: Record<string, number>;
    };
    const pick = (m: Record<string, number>, prefix: string) =>
      Object.fromEntries(
        Object.entries(m)
          .filter(([k]) => k.startsWith(prefix))
          .map(([k, v]) => [k.slice(prefix.length), v]),
      );
    $("#metrics").replaceChildren(
      ...metricRows.map(([name, prefix]) => {
        const c = pick(counters, prefix);
        const g = pick(gauges, prefix);
        const tr = el("tr", "", { "data-route": name });
        const cell = (v: number) => el("td", Number.isNaN(v) ? "–" : String(v));
        tr.append(el("th", name), ...metricColumns.map(([, get]) => cell(get(c, g))));
        if (name === "native")
          (tr.children[2] as HTMLElement).textContent = `${c.watch_requests ?? 0} (${c.watch_resumed ?? 0})`;
        return tr;
      }),
    );
    $("#refused-native").textContent = String(counters["native.refused_session_requests"] ?? 0);
  } catch {
    // The host is gone or restarting; the next poll tries again.
  }
}
$("#metrics-head").replaceChildren(el("th", "source"), ...metricColumns.map(([name]) => el("th", name)));
setInterval(pollMetrics, 2000);
void pollMetrics();

// ------------------------------------------------- a refused session never falls back to native --

$("#refused-open").addEventListener("click", () => {
  const out = $("#refused-result");
  out.textContent = "connecting as the refused session…";
  // A second identity gets its own store, and only ever the source it asked for. When the gateway
  // refuses it, the page reports the refusal and stops: it does not try /k8s instead.
  const store = new LiveResourceStore();
  const handle = connectResourceStream(
    resourceStreamURL("/stream/full", widgetScope),
    (event) => applyStreamEvent(store, event),
    {
      headers: { "X-Compare-Session": config.refusedSession },
      onError: (code, message, terminal) => {
        out.textContent = `gateway refused: ${code}${terminal ? " (terminal)" : ""} — ${message}. No native request was made.`;
        out.dataset.code = code;
      },
    },
  );
  handle.closed
    .catch(() => {})
    .then(() => {
      out.dataset.final = handle.state.status;
      void pollMetrics();
    });
});
