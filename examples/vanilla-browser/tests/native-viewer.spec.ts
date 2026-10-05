// The native viewer example (examples/native-viewer) in a real browser, on both entry points.
//
// No cluster: Playwright plays the host's /k8s proxy on the page's own origin, answering each LIST
// and WATCH itself, so the test decides when a watch is accepted and what it says. It also serves the
// example page and the built library from the repository, where the page imports them by relative
// path, exactly as `kubectl proxy --www` does in the example's README.

import { readFile } from "node:fs/promises";
import { extname } from "node:path";
import { fileURLToPath } from "node:url";
import type { Page } from "@playwright/test";
import { expect, test } from "./fixtures.ts";

const repository = fileURLToPath(new URL("../../../", import.meta.url));
const types: Record<string, string> = { ".html": "text/html", ".js": "text/javascript" };

async function serveFromRepository(page: Page) {
  for (const prefix of ["/examples/native-viewer/", "/packages/krm-stream/dist/"]) {
    await page.route(`**${prefix}**`, async (route) => {
      const path = new URL(route.request().url()).pathname;
      await route.fulfill({
        body: await readFile(`${repository}${path.slice(1)}`),
        contentType: types[extname(path)] ?? "application/octet-stream",
      });
    });
  }
}

/** A ConfigMap as a typed LIST returns it: no apiVersion or kind on the item. */
const item = (name: string, uid: string, rv: string, value: string) => ({
  metadata: { name, uid, namespace: "app", resourceVersion: rv, labels: { tier: "web" } },
  data: { value },
});
const typed = (object: ReturnType<typeof item>) => ({ apiVersion: "v1", kind: "ConfigMap", ...object });
const frames = (...events: unknown[]) => events.map((e) => `${JSON.stringify(e)}\n`).join("");

function gate() {
  let open!: () => void;
  const opened = new Promise<void>((resolve) => {
    open = resolve;
  });
  return { opened, open };
}

test("the native viewer lists, goes live on acceptance, resumes after EOF, re-lists after expiry and disconnects", async ({
  page,
  entry,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await serveFromRepository(page);

  const a1 = item("a", "uid-a", "1", "one");
  const a2 = item("a", "uid-a", "11", "two");
  const b = item("b", "uid-b", "1", "one");
  const bDeleted = item("b", "uid-b", "21", "one");
  const c = item("c", "uid-c", "12", "new");
  const bookmark = {
    type: "BOOKMARK",
    object: { kind: "ConfigMap", apiVersion: "v1", metadata: { resourceVersion: "20" } },
  };
  const expired = {
    type: "ERROR",
    object: {
      kind: "Status",
      apiVersion: "v1",
      status: "Failure",
      code: 410,
      reason: "Expired",
      message: "too old resource version: 20 (25)",
    },
  };
  // Each WATCH the page sends: the resourceVersion it must ask for, and what it is answered once
  // its gate opens. A fulfilled body ends, so every answered watch is followed by EOF.
  const script = [
    // The snapshot's watch: two changes and a bookmark, then EOF. The page resumes from the bookmark.
    {
      from: "10",
      gate: gate(),
      body: frames({ type: "MODIFIED", object: typed(a2) }, { type: "ADDED", object: typed(c) }, bookmark),
    },
    // The resumed watch replays b's deletion, then the history expires.
    { from: "20", gate: gate(), body: frames({ type: "DELETED", object: typed(bDeleted) }, expired) },
    // The replacement snapshot's watch, then a resume from its list version that is never answered.
    { from: "30", gate: gate(), body: "" },
    { from: "30", gate: undefined, body: "" },
  ];
  let lists = 0;
  let watchCount = 0;
  await page.route("**/k8s/**", async (route) => {
    const url = new URL(route.request().url());
    expect(url.pathname).toBe("/k8s/api/v1/namespaces/app/configmaps");
    expect(url.searchParams.get("labelSelector"), "LIST and WATCH share the selectors").toBe("tier=web");
    if (url.searchParams.get("watch") !== "1") {
      lists++;
      // The first snapshot holds a and b. The second holds only a: c was deleted while the history
      // was expired, and nothing reports it except that complete snapshot.
      const items = lists === 1 ? [a1, b] : [a2];
      await route.fulfill({
        json: {
          kind: "ConfigMapList",
          apiVersion: "v1",
          metadata: { resourceVersion: `${lists === 1 ? 1 : 3}0` },
          items,
        },
      });
      return;
    }
    const step = script[watchCount++];
    expect(url.searchParams.get("resourceVersion")).toBe(step?.from);
    expect(url.searchParams.get("allowWatchBookmarks")).toBe("true");
    if (!step?.gate) return; // never answered: the page waits, connecting, until it disconnects
    await step.gate.opened;
    await route.fulfill({ contentType: "application/json", body: step.body });
  });

  await page.goto(`/examples/native-viewer/index.html?entry=${entry}&namespace=app&labelSelector=tier%3Dweb`);
  const row = (uid: string) => page.locator(`tr[data-uid="${uid}"]`);
  const state = page.locator("#state");
  const history = page.locator("#history");

  // The snapshot is applied, but the watch is still opening: not live yet.
  await expect(row("uid-a")).toContainText("v1 ConfigMap");
  await expect(row("uid-b")).toBeVisible();
  await expect(state).toHaveText("syncing");
  await expect(history).not.toContainText("live");

  script[0]!.gate!.open();
  await expect(history).toContainText("syncing → live");
  await expect(row("uid-a")).toContainText('"value": "two"');
  await expect(row("uid-c")).toBeVisible();
  // The watch ended. The page resumes it from the bookmark instead of listing again: while the
  // resumed watch is opening it is connecting, never syncing, and it keeps every row.
  await expect.poll(() => watchCount).toBe(2);
  await expect(state).toHaveText("connecting");
  expect(lists, "a resume is not a snapshot").toBe(1);
  await expect(row("uid-b")).toBeVisible();

  script[1]!.gate!.open();
  // Accepted: live at once, and the missed deletion arrives. Then the history expires.
  await expect(row("uid-b")).toHaveCount(0);
  await expect(page.locator("#error")).toHaveText("RESYNC_REQUIRED: too old resource version: 20 (25)");
  await expect.poll(() => lists).toBe(2);
  // The replacement snapshot is incomplete until its watch is accepted, so c — missing from it — is
  // still shown, and the page is syncing.
  await expect(state).toHaveText("syncing");
  await expect(row("uid-c")).toBeVisible();

  script[2]!.gate!.open();
  // The replacement snapshot completes: c is pruned. Its watch ends, and the next one resumes.
  await expect(row("uid-c")).toHaveCount(0);
  await expect(row("uid-a")).toBeVisible();
  await expect.poll(() => watchCount).toBe(4);
  await expect(state).toHaveText("connecting");
  await expect(history).toHaveText(
    [
      ...["connecting", "syncing", "syncing", "live"],
      ...["retrying", "connecting", "live"],
      ...["retrying", "connecting", "syncing", "syncing", "live"],
      ...["retrying", "connecting"],
    ].join(" → "),
  );
  expect(lists).toBe(2);

  await page.getByRole("button", { name: "Disconnect" }).click();
  await expect(state).toHaveText("closed");
  const requested = lists + watchCount;
  await page.waitForTimeout(1_000);
  expect(lists + watchCount, "nothing is requested after disconnect").toBe(requested);
  await expect(page.locator("#error")).toHaveText("");
  expect(errors).toEqual([]);
});

test("a refused native list is terminal and shows the proxy's Status message", async ({ page, entry }) => {
  await serveFromRepository(page);
  let requests = 0;
  await page.route("**/k8s/**", async (route) => {
    requests++;
    await route.fulfill({
      status: 403,
      json: { kind: "Status", apiVersion: "v1", status: "Failure", code: 403, message: "configmaps is forbidden" },
    });
  });
  await page.goto(`/examples/native-viewer/index.html?entry=${entry}&namespace=app`);
  await expect(page.locator("#state")).toHaveText("terminal");
  await expect(page.locator("#error")).toHaveText("FORBIDDEN (terminal): configmaps is forbidden");
  expect(requests).toBe(1);
});
