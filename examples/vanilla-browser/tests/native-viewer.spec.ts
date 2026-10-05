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

test("the native viewer lists, goes live once the watch is accepted, re-lists and disconnects", async ({
  page,
  entry,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await serveFromRepository(page);

  const a1 = item("a", "uid-a", "1", "one");
  const a2 = item("a", "uid-a", "11", "two");
  const b = item("b", "uid-b", "1", "one");
  const c = item("c", "uid-c", "12", "new");
  const watches = [gate(), gate()];
  let lists = 0;
  let watchCount = 0;
  await page.route("**/k8s/**", async (route) => {
    const url = new URL(route.request().url());
    expect(url.pathname).toBe("/k8s/api/v1/namespaces/app/configmaps");
    expect(url.searchParams.get("labelSelector"), "LIST and WATCH share the selectors").toBe("tier=web");
    if (url.searchParams.get("watch") !== "1") {
      lists++;
      // The first snapshot holds a and b. Every later one holds only a: c is deleted while the page
      // is between watches, and nothing reports it except the next complete snapshot.
      const items = lists === 1 ? [a1, b] : [a2];
      await route.fulfill({
        json: { kind: "ConfigMapList", apiVersion: "v1", metadata: { resourceVersion: `${lists}0` }, items },
      });
      return;
    }
    const n = watchCount++;
    expect(url.searchParams.get("resourceVersion")).toBe(`${n + 1}0`);
    const held = watches[n];
    if (!held) return; // a later watch is never answered: the page stays syncing until it disconnects
    await held.opened;
    // A fulfilled body ends, so each answered watch is followed by EOF and a fresh LIST.
    await route.fulfill({
      contentType: "application/json",
      body:
        n === 0
          ? frames(
              { type: "MODIFIED", object: typed(a2) },
              { type: "DELETED", object: typed(b) },
              { type: "ADDED", object: typed(c) },
            )
          : "",
    });
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

  watches[0]!.open();
  await expect(history).toContainText("syncing → live");
  // The watch's events arrived; then it ended, and the next LIST is incomplete until its watch is
  // accepted, so c — missing from that LIST — is still shown.
  await expect.poll(() => lists).toBe(2);
  await expect(state).toHaveText("syncing");
  await expect(row("uid-a")).toContainText('"value": "two"');
  await expect(row("uid-b")).toHaveCount(0);
  await expect(row("uid-c")).toBeVisible();

  watches[1]!.open();
  // The replacement snapshot completes: c is pruned.
  await expect(row("uid-c")).toHaveCount(0);
  await expect(row("uid-a")).toBeVisible();
  await expect.poll(() => lists).toBe(3);
  await expect(state).toHaveText("syncing");

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
