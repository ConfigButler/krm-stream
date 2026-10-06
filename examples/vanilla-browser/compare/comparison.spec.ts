// The comparison page against the real comparison host and API server, on both entry points. Run by
// `task compare-browser`; see ../compare.config.ts.
//
// The regression this guards: another writer changes a field the person has FOCUSED but not edited.
// The page must show the new value in the focused input, so the next keystroke extends it. An input
// left showing the old text saved that old text plus the keystroke, at the newer version and with no
// conflict — silently overwriting the other writer.

import type { APIRequestContext } from "@playwright/test";
import { expect, test } from "../tests/fixtures.ts";

const admin = { "X-Compare-Session": "viewer-session", "Content-Type": "application/json" };

async function call(request: APIRequestContext, path: string, data?: unknown) {
  const res = await request.post(`http://127.0.0.1:8120${path}`, { headers: admin, data });
  expect(res.ok(), `${path}: HTTP ${res.status()} ${await res.text()}`).toBe(true);
}

async function clusterNote(request: APIRequestContext, name: string): Promise<unknown> {
  const res = await request.get("http://127.0.0.1:8120/admin/state", { headers: admin });
  const state = (await res.json()) as { kind: string; name: string; spec?: { note?: unknown } }[];
  return state.find((o) => o.kind === "Widget" && o.name === name)?.spec?.note;
}

for (const source of ["native", "full", "spec"] as const) {
  test(`${source}: a focused field follows another writer, and the next keystroke extends the new value`, async ({
    page,
    request,
    entry,
  }) => {
    const errors: string[] = [];
    page.on("pageerror", (e) => errors.push(e.message));
    await call(request, "/admin/reset");
    await page.goto(`/login?entry=${entry}`);

    const column = page.locator(`[data-source=${source}]`);
    await expect(column.locator("[data-role=state]")).toHaveText("live");
    const widget = await page.locator("#widget").inputValue();
    const note = column.locator("[data-role=note]");
    const initial = await clusterNote(request, widget);
    await expect(note).toHaveValue(String(initial));

    await note.click();
    await note.press("End");
    const external = `external-${source}-${entry}`;
    await call(request, "/admin/write", { name: widget, note: external });

    // The watch delivers it and the focused input shows it, still focused.
    await expect(note).toHaveValue(external);
    await expect(note).toBeFocused();
    await note.press("End");
    await note.pressSequentially("!");
    await expect(column.locator("[data-role=conflicts] li")).toHaveCount(0);

    await column.locator("[data-role=save]").click();
    await expect(column.locator("[data-role=outcome]")).toHaveText("saved");
    await expect.poll(() => clusterNote(request, widget)).toBe(`${external}!`);
    expect(errors).toEqual([]);
  });
}
