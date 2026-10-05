// The native editor example (examples/native-editor) in a real browser, on both entry points: the
// page, connectNativeWatch, LiveResourceStore and the native editor, against a host proxy that keeps
// its watch open while the person edits.
//
// No cluster. Playwright's request routing cannot stream a response, so each test starts a small
// HTTP server that plays both halves of `kubectl proxy --www`: it serves the page and the built
// library from the repository, and answers /k8s as an API server would for one ConfigMap — LIST,
// a WATCH that stays open, GET, and a conditional JSON merge PATCH refused like the Go test proxy
// refuses it. A test can hold an answer, script it, or hold watch events back, so it decides what
// the API server did and when the watch saw it.

import { readFile } from "node:fs/promises";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import type { AddressInfo } from "node:net";
import { extname } from "node:path";
import { fileURLToPath } from "node:url";
import type { Page } from "@playwright/test";
import { test as base, expect } from "./fixtures.ts";

const repository = fileURLToPath(new URL("../../../", import.meta.url));
const types: Record<string, string> = { ".html": "text/html", ".js": "text/javascript" };
const LAST_APPLIED = "kubectl.kubernetes.io/last-applied-configuration";
const OBJECT = "/k8s/api/v1/namespaces/app/configmaps/settings";
const COLLECTION = "/k8s/api/v1/namespaces/app/configmaps";

interface ConfigMap {
  metadata: {
    name: string;
    namespace: string;
    uid: string;
    resourceVersion: string;
    annotations: Record<string, string>;
    managedFields: unknown[];
  };
  data: Record<string, string>;
}

interface Call {
  method: string;
  path: string;
  query: URLSearchParams;
  contentType: string | undefined;
  body: unknown;
}

type Answer = (call: Call, res: ServerResponse) => void | Promise<void>;

/** One ConfigMap behind a host proxy. Writes are answered in order by `patches` and reads by
 * `reads`, falling back to the API server's own behavior. */
class Host {
  readonly calls: Call[] = [];
  readonly patches: Answer[] = [];
  readonly reads: Answer[] = [];
  object: ConfigMap | undefined = configMap("u1", 1, "base");
  #rv = 1;
  #watches = new Set<ServerResponse>();
  #held: string[] = [];
  #server: Server;
  url = "";

  constructor() {
    this.#server = createServer((req, res) => void this.#serve(req, res));
  }

  async start() {
    await new Promise<void>((resolve) => this.#server.listen(0, "127.0.0.1", resolve));
    this.url = `http://127.0.0.1:${(this.#server.address() as AddressInfo).port}`;
  }

  async stop() {
    for (const watch of this.#watches) watch.end();
    this.#server.closeAllConnections();
    await new Promise((resolve) => this.#server.close(resolve));
  }

  get openWatches() {
    return this.#watches.size;
  }

  /** Requests for the object itself: the editor's PATCHes and guarded GETs. */
  objectCalls() {
    return this.calls.filter((c) => c.path === OBJECT).map((c) => c.method);
  }

  patchBodies() {
    return this.calls.filter((c) => c.method === "PATCH").map((c) => c.body);
  }

  /** Another writer changes the object. Its watch event is delivered now, or held until flush(). */
  change(mutate: (object: ConfigMap) => void, { deliver = true } = {}) {
    mutate(this.object!);
    this.object!.metadata.resourceVersion = String(++this.#rv);
    this.#emit("MODIFIED", this.object!, deliver);
  }

  /** Delete the object, with nothing in its place. */
  remove() {
    const old = this.object!;
    old.metadata.resourceVersion = String(++this.#rv);
    this.object = undefined;
    this.#emit("DELETED", old, true);
  }

  /** Delete the object and create another under the same name, with a new UID. */
  replace({ deliver = true } = {}) {
    const old = this.object!;
    old.metadata.resourceVersion = String(++this.#rv);
    this.#emit("DELETED", old, deliver);
    this.object = configMap("u2", ++this.#rv, "replacement");
    this.#emit("ADDED", this.object, deliver);
  }

  /** Deliver the watch events held back so far. */
  flush() {
    for (const frame of this.#held.splice(0)) for (const watch of this.#watches) watch.write(frame);
  }

  /** The API server's PATCH: preconditions checked, then the merge patch applied and echoed. */
  patch = (call: Call, res: ServerResponse, { deliver = true } = {}) => {
    const body = call.body as { metadata?: Record<string, unknown> };
    const object = this.object;
    if (!object) return status(res, 404, 'configmaps "settings" not found');
    if (
      body.metadata?.resourceVersion !== object.metadata.resourceVersion ||
      body.metadata?.uid !== object.metadata.uid
    ) {
      return status(res, 409, "Operation cannot be fulfilled: the object has been modified");
    }
    this.write(body, { deliver });
    json(res, 200, typed(this.object!));
  };

  /** Apply a merge patch as the API server would, without answering. */
  write(body: unknown, { deliver = true } = {}) {
    const { metadata, ...rest } = body as { metadata: Record<string, unknown> };
    const { uid: _uid, resourceVersion: _rv, ...fields } = metadata;
    this.object = merge(this.object, { ...rest, metadata: fields }) as ConfigMap;
    this.object.metadata.resourceVersion = String(++this.#rv);
    this.#emit("MODIFIED", this.object, deliver);
  }

  #emit(type: string, object: ConfigMap, deliver: boolean) {
    const frame = `${JSON.stringify({ type, object: typed(object) })}\n`;
    if (!deliver) return void this.#held.push(frame);
    for (const watch of this.#watches) watch.write(frame);
  }

  async #serve(req: IncomingMessage, res: ServerResponse) {
    const url = new URL(req.url!, this.url);
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    if (!url.pathname.startsWith("/k8s/")) return this.#file(url.pathname, res);
    const text = Buffer.concat(chunks).toString();
    const call: Call = {
      method: req.method!,
      path: url.pathname,
      query: url.searchParams,
      contentType: req.headers["content-type"],
      body: text ? JSON.parse(text) : undefined,
    };
    this.calls.push(call);

    if (call.path === COLLECTION && call.method === "GET") {
      expect(call.query.get("fieldSelector")).toBe("metadata.name=settings");
      if (call.query.get("watch") === "1") {
        // Accepted, and open until the page or the test closes it.
        res.writeHead(200, { "Content-Type": "application/json" });
        res.flushHeaders();
        this.#watches.add(res);
        res.on("close", () => this.#watches.delete(res));
        return;
      }
      const items = this.object ? [this.object] : [];
      return json(res, 200, {
        kind: "ConfigMapList",
        apiVersion: "v1",
        metadata: { resourceVersion: String(this.#rv) },
        items,
      });
    }
    if (call.path !== OBJECT) return status(res, 404, "not proxied");
    if (call.method === "GET") {
      const answer = this.reads.shift();
      if (answer) return answer(call, res);
      return this.object ? json(res, 200, typed(this.object)) : status(res, 404, "not found");
    }
    if (call.method !== "PATCH") return status(res, 405, "not proxied");
    // The host proxy's own checks, as in gateway/kube/native_e2e_test.go.
    if (call.contentType !== "application/merge-patch+json") return status(res, 415, "only merge patches");
    const metadata = (
      call.body as {
        metadata?: { uid?: string; resourceVersion?: string; annotations?: object; managedFields?: unknown };
      }
    ).metadata;
    if (!metadata?.uid || !metadata.resourceVersion) return status(res, 400, "a native patch needs both preconditions");
    if (metadata.managedFields !== undefined || (metadata.annotations && LAST_APPLIED in metadata.annotations)) {
      return status(res, 400, "a native patch must not touch machinery");
    }
    const answer = this.patches.shift();
    return answer ? answer(call, res) : this.patch(call, res);
  }

  async #file(path: string, res: ServerResponse) {
    if (!path.startsWith("/examples/native-editor/") && !path.startsWith("/packages/krm-stream/dist/")) {
      return status(res, 404, "not served");
    }
    res.writeHead(200, { "Content-Type": types[extname(path)] ?? "application/octet-stream", Connection: "close" });
    res.end(await readFile(`${repository}${path.slice(1)}`));
  }
}

function configMap(uid: string, rv: number, value: string): ConfigMap {
  return {
    metadata: {
      name: "settings",
      namespace: "app",
      uid,
      resourceVersion: String(rv),
      annotations: { owner: "team-a", [LAST_APPLIED]: `{"data":{"value":"${value}"}}` },
      managedFields: [{ manager: "kubectl", operation: "Apply" }],
    },
    data: { value },
  };
}

const typed = (object: ConfigMap) => ({ apiVersion: "v1", kind: "ConfigMap", ...structuredClone(object) });

function merge(target: unknown, patch: unknown): unknown {
  if (patch === null || typeof patch !== "object" || Array.isArray(patch)) return patch;
  const out: Record<string, unknown> =
    target && typeof target === "object" && !Array.isArray(target) ? { ...(target as object) } : {};
  for (const [k, v] of Object.entries(patch)) {
    if (v === null) delete out[k];
    else out[k] = merge(out[k], v);
  }
  return out;
}

// Every answer but a watch closes its connection, so Chromium never resends a request over a reused
// socket and the PATCH count is the page's own.
function json(res: ServerResponse, code: number, body: unknown) {
  res.writeHead(code, { "Content-Type": "application/json", Connection: "close" });
  res.end(JSON.stringify(body));
}

function status(res: ServerResponse, code: number, message: string, extra: object = {}) {
  json(res, code, { kind: "Status", apiVersion: "v1", status: "Failure", code, message, ...extra });
}

function gate() {
  let open!: () => void;
  const opened = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { opened, open };
}

const test = base.extend<{ host: Host; errors: string[] }>({
  host: async ({}, use) => {
    const host = new Host();
    await host.start();
    await use(host);
    await host.stop();
  },
  // Every test: the page reports no uncaught error.
  errors: [
    async ({ page }, use) => {
      const errors: string[] = [];
      page.on("pageerror", (e) => errors.push(e.message));
      await use(errors);
      expect(errors).toEqual([]);
    },
    { auto: true },
  ],
});

async function open(page: Page, host: Host, entry: string, query = "") {
  await page.goto(`${host.url}/examples/native-editor/index.html?entry=${entry}&namespace=app&name=settings${query}`);
  await expect(page.locator("#state")).toHaveText("live");
  await expect(field(page, "data value")).toHaveValue("base");
}

const field = (page: Page, name: string) => page.getByRole("textbox", { name, exact: true });
const outcome = (page: Page) => page.locator("#outcome");
const save = (page: Page) => page.getByRole("button", { name: "Save" });
const confirm = (page: Page) => page.getByRole("button", { name: "Confirm current state" });

test("a conditional save settles on its watch echo, with machinery read-only", async ({ page, entry, host }) => {
  await open(page, host, entry);
  await expect(page.locator("#identity")).toContainText("u1");
  await expect(page.locator("#server")).toContainText("kubectl (Apply)");
  // The last-applied annotation is shown but has no input; managedFields never reach the form.
  await expect(field(page, `annotation ${LAST_APPLIED}`)).toHaveCount(0);
  await expect(page.locator("#fields tr", { hasText: LAST_APPLIED })).toContainText("read-only");

  await field(page, "data value").fill("mine");
  await field(page, "annotation owner").fill("team-b");
  await expect(page.locator("#changes")).toContainText('change data value: "base" → "mine"');
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  await expect(outcome(page)).toContainText("resourceVersion 2. Every field matches the server.");
  expect(host.calls.find((c) => c.method === "PATCH")?.contentType).toBe("application/merge-patch+json");
  expect(host.patchBodies()).toEqual([
    { metadata: { annotations: { owner: "team-b" }, uid: "u1", resourceVersion: "1" }, data: { value: "mine" } },
  ]);
  expect(host.objectCalls()).toEqual(["PATCH"]);
  expect(host.object!.metadata.annotations[LAST_APPLIED]).toBe('{"data":{"value":"base"}}');
  await expect(page.locator("#changes")).toHaveText("No pending edits.");
  await expect(page.locator("#identity")).toContainText("resourceVersion2");
});

test("typing and focus survive a save in flight and its echo", async ({ page, entry, host }) => {
  await open(page, host, entry);
  await field(page, "data value").fill("first");
  const held = gate();
  host.patches.push(async (call, res) => {
    await held.opened;
    host.patch(call, res);
  });
  await save(page).click();
  await expect(save(page)).toBeDisabled(); // one request at a time

  // Typing while the write is in flight: into the field being saved, then into another one.
  await field(page, "data value").click();
  await page.keyboard.press("End");
  await page.keyboard.type("+1");
  await field(page, "annotation owner").click();
  await page.keyboard.press("End");
  await page.keyboard.type(" and more");
  held.open();

  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  expect(host.patchBodies()).toEqual([{ metadata: { uid: "u1", resourceVersion: "1" }, data: { value: "first" } }]);
  await expect(field(page, "annotation owner")).toBeFocused();
  // An unrelated change arrives while the person is still typing: a new row, the caret stays put.
  host.change((o) => {
    o.data.extra = "from the server";
  });
  await expect(field(page, "data extra")).toHaveValue("from the server");
  await page.keyboard.type("!");
  await expect(field(page, "annotation owner")).toHaveValue("team-a and more!");
  await expect(field(page, "annotation owner")).toBeFocused();

  // The echo of "first" arrived after "+1" was typed into the same field: the server value moved
  // under a local edit, which the page shows as a conflict rather than choosing for the person.
  await expect(field(page, "data value")).toHaveValue("first+1");
  await expect(page.locator("#conflicts")).toContainText('data value — yours: "first+1", server: "first"');
  await expect(save(page)).toBeDisabled();
  await page.getByRole("button", { name: "Keep mine" }).click();
  await expect(page.locator("#changes")).toContainText('change data value: "first" → "first+1"');
  await expect(page.locator("#changes")).toContainText('change annotation owner: "team-a" → "team-a and more!"');
  await expect(save(page)).toBeEnabled();
});

test("a 409 refreshes the base, keeps the edits and writes only on the next deliberate Save", async ({
  page,
  entry,
  host,
}) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.patches.push((call, res) => {
    // Another writer lands between capture and PATCH; the watch has not delivered it yet.
    host.change(
      (o) => {
        o.metadata.annotations.owner = "team-c";
      },
      { deliver: false },
    );
    host.patch(call, res);
  });
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "version-stale");
  await expect(outcome(page)).toContainText("nothing was written");
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
  await expect(field(page, "data value")).toHaveValue("mine");
  await expect(field(page, "annotation owner")).toHaveValue("team-c");
  await expect(page.locator("#identity")).toContainText("resourceVersion2");
  await page.waitForTimeout(500);
  expect(host.objectCalls(), "no automatic retry").toEqual(["PATCH", "GET"]);
  host.flush();

  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  expect(host.patchBodies()[1]).toEqual({ metadata: { uid: "u1", resourceVersion: "2" }, data: { value: "mine" } });
  expect(host.object!.data.value).toBe("mine");
  expect(host.object!.metadata.annotations.owner).toBe("team-c");
});

test("field conflicts are resolved explicitly before the next save", async ({ page, entry, host }) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  await field(page, "annotation owner").fill("team-b");
  host.patches.push((call, res) => {
    host.change(
      (o) => {
        o.data.value = "theirs";
        o.metadata.annotations.owner = "team-c";
      },
      { deliver: false },
    );
    host.patch(call, res);
  });
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "draft-conflict");
  const conflicts = page.locator("#conflicts li");
  await expect(conflicts).toHaveCount(2);
  await expect(save(page)).toBeDisabled();
  await expect(page.locator("#hint")).toHaveText("Resolve the conflicts to save.");
  await conflicts.filter({ hasText: "data value" }).getByRole("button", { name: "Keep mine" }).click();
  await conflicts.filter({ hasText: "annotation owner" }).getByRole("button", { name: "Use server value" }).click();
  await expect(page.locator("#conflicts-section")).toBeHidden();
  await expect(field(page, "annotation owner")).toHaveValue("team-c");

  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  expect(host.patchBodies()[1]).toEqual({ metadata: { uid: "u1", resourceVersion: "2" }, data: { value: "mine" } });
  expect(host.object!.metadata.annotations.owner).toBe("team-c");
});

test("a lost response is settled by a GET, never a second PATCH", async ({ page, entry, host }) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.patches.push((call, res) => {
    // The write lands, but its answer and its watch event never reach the page.
    host.write(call.body, { deliver: false });
    res.socket?.destroy();
  });
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "unknown");
  await expect(outcome(page)).toContainText("got no answer");
  await expect(confirm(page)).toBeVisible();
  await page.waitForTimeout(500);
  expect(host.objectCalls(), "no automatic retry").toEqual(["PATCH"]);

  // A deliberate Save reads instead of writing again, and finds the write landed.
  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "confirmed");
  await expect(outcome(page)).toContainText("resourceVersion 2. Every field matches the server.");
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
  await expect(confirm(page)).toBeHidden();
  host.flush();
  await expect(page.locator("#changes")).toHaveText("No pending edits.");
});

test("a proxy 502 is confirmed by a GET, and the unsaved edit is written by the next Save", async ({
  page,
  entry,
  host,
}) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.patches.push((_call, res) => status(res, 502, "upstream unavailable"));
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "unknown");
  await expect(outcome(page)).toContainText("HTTP 502");
  await confirm(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "confirmed");
  await expect(outcome(page)).toContainText("1 field still differ");
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
  await expect(page.locator("#changes")).toContainText('change data value: "base" → "mine"');

  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  expect(host.objectCalls()).toEqual(["PATCH", "GET", "PATCH"]);
});

test("an accepted write without an echo is confirmed explicitly, and its unsaved field stays dirty", async ({
  page,
  entry,
  host,
}) => {
  await open(page, host, entry, "&echoWaitMs=1000");
  await field(page, "data value").fill("mine");
  // Accepted, but an admission webhook restored the value: the object is unchanged, so no event.
  host.patches.push((_call, res) => json(res, 200, typed(host.object!)));
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "saved");
  await expect(outcome(page)).toHaveAttribute("data-kind", "no-echo");
  await expect(confirm(page)).toBeVisible();
  await page.waitForTimeout(500);
  expect(host.objectCalls(), "no automatic retry or read").toEqual(["PATCH"]);

  await confirm(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "confirmed");
  await expect(outcome(page)).toContainText("1 field still differ from the server and were not saved by this write");
  await expect(confirm(page)).toBeHidden();
  await expect(page.locator("#changes")).toContainText('change data value: "base" → "mine"');
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
});

test("refused writes keep the draft and need no read", async ({ page, entry, host }) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.patches.push((_call, res) => status(res, 403, 'configmaps "settings" is forbidden'));
  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "forbidden");
  await expect(outcome(page)).toContainText('HTTP 403: configmaps "settings" is forbidden. Nothing was written');

  host.patches.push((_call, res) =>
    status(res, 422, "ConfigMap is invalid", { details: { causes: [{ field: "data.value", message: "too long" }] } }),
  );
  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "invalid");
  await expect(outcome(page)).toContainText("(data.value: too long)");
  await expect(field(page, "data value")).toHaveValue("mine");
  expect(host.objectCalls()).toEqual(["PATCH", "PATCH"]);
});

test("an object replaced under the same name is never written, and the replacement opens separately", async ({
  page,
  entry,
  host,
}) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.patches.push((call, res) => {
    host.replace({ deliver: false });
    host.patch(call, res);
  });
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "unavailable");
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
  expect(host.object!.data.value).toBe("replacement");

  // The watch catches up: the edited object is gone and its edits are kept for copying.
  host.flush();
  await expect(page.locator("#removed")).toBeVisible();
  await expect(page.locator("#recovery")).toHaveText('change data value: "base" → "mine"');
  await expect(page.locator("#identity")).toContainText("Removed from the server.");
  await expect(save(page)).toBeDisabled();

  await page.getByRole("button", { name: "Discard these edits and open the replacement" }).click();
  await expect(page.locator("#identity")).toContainText("u2");
  await expect(field(page, "data value")).toHaveValue("replacement");
  await expect(page.locator("#changes")).toHaveText("No pending edits.");
  expect(host.objectCalls()).toEqual(["PATCH", "GET"]);
});

test("disconnect closes the watch, clears the echo timer and ignores a late answer", async ({ page, entry, host }) => {
  await open(page, host, entry, "&echoWaitMs=1500");
  await field(page, "data value").fill("mine");
  host.patches.push((_call, res) => json(res, 200, typed(host.object!)));
  await save(page).click();
  await expect(outcome(page)).toHaveAttribute("data-kind", "saved");

  // No echo yet, so the next Save is a guarded read. Hold it, and disconnect while it is in flight.
  const held = gate();
  host.reads.push(async (_call, res) => {
    await held.opened;
    json(res, 200, typed(host.object!));
  });
  await save(page).click();
  await expect.poll(() => host.objectCalls()).toEqual(["PATCH", "GET"]);
  await page.getByRole("button", { name: "Disconnect" }).click();

  await expect(page.locator("#state")).toHaveText("closed");
  await expect(page.locator("#outcome")).toHaveAttribute("data-kind", "closed");
  await expect(field(page, "data value")).toBeDisabled();
  await expect(save(page)).toBeDisabled();
  await expect.poll(() => host.openWatches).toBe(0);
  const requests = host.calls.length;
  held.open();
  await page.waitForTimeout(2_000); // past the echo wait
  await expect(page.locator("#outcome")).toHaveAttribute("data-kind", "closed");
  await expect(confirm(page)).toBeHidden();
  expect(host.calls.length, "nothing is requested after disconnect").toBe(requests);
});

test("multiline values keep their line breaks through editing and saving", async ({ page, entry, host }) => {
  host.change((o) => {
    o.data.script = "first\nsecond\n";
  });
  await open(page, host, entry);
  const script = field(page, "data script");
  await expect(script).toHaveValue("first\nsecond\n");
  await script.click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("third");
  await field(page, "annotation owner").fill("team-a\nteam-b");
  await save(page).click();

  await expect(outcome(page)).toHaveAttribute("data-kind", "echoed");
  expect(host.patchBodies()).toEqual([
    {
      metadata: { annotations: { owner: "team-a\nteam-b" }, uid: "u1", resourceVersion: "2" },
      data: { script: "first\nsecond\nthird" },
    },
  ]);
  await expect(script).toHaveValue("first\nsecond\nthird");
});

test("a deleted object shows its unsaved edits at once, with nothing to open in its place", async ({
  page,
  entry,
  host,
}) => {
  await open(page, host, entry);
  await field(page, "data value").fill("mine");
  host.remove();

  // The deletion is the last event: no later render could fill the copy in.
  await expect(page.locator("#removed")).toBeVisible();
  await expect(page.locator("#recovery")).toHaveText('change data value: "base" → "mine"');
  await expect(page.locator("#identity")).toContainText("Removed from the server.");
  await expect(page.getByRole("button", { name: "Discard these edits and open the replacement" })).toBeHidden();
  await expect(save(page)).toBeDisabled();
  expect(host.objectCalls()).toEqual([]);
});
