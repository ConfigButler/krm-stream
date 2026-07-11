// The top of the ladder that needs no cluster: a REAL browser, a REAL EventSource, a REAL unbundled
// ESM import, against the REAL gateway.
//
// Everything below this rung feeds the store bytes. That proves the protocol and the merge, and it
// cannot prove any of the following — each of which breaks the product, and each of which is
// invisible to `node --test`:
//
//   - the published ESM actually imports in a browser with NO BUNDLER (dist/index.js imports
//     ./store.js imports ./merge.js…). Node importing it proves nothing: Node is not a browser.
//   - native EventSource works at all — it is the same-origin cookie path, the v1 baseline (spec §7),
//     and the only transport a plain <script type="module"> can use.
//   - a read-only region actually FLASHES. "Read-only is not ignored" is the entire product thesis,
//     and it is a DOM fact, not a data-structure fact.
//   - a user can type into a field while the server changes the object underneath them, and keep
//     what they typed.

import { expect, test } from "@playwright/test";

const path = (...segments: (string | number)[]) => JSON.stringify(segments);

test("the built ESM imports in a browser with no bundler at all", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));

  await page.goto("/?fixture=snapshot-then-deltas&pace=0ms");
  await expect(page.locator("#status-line")).toHaveText(/synced/);

  // If a single bare specifier or a missing extension had crept into the emitted JS, the module graph
  // would have failed to load and this page would be blank. That is the whole constraint, checked.
  expect(errors, `the browser could not load the library:\n${errors.join("\n")}`).toEqual([]);
});

test("status follows the server live and FLASHES what moved — and never becomes an edit", async ({ page }) => {
  // deploy-web.v1 -> v2 is a status-only change: readyReplicas 1 -> 2, Available False -> True.
  // The spec is byte-identical. This is the dominant traffic on a real stream, and the demo.
  await page.goto("/?fixture=status-only-churn&pace=250ms");

  const ready = page.getByTestId(`value:${path("status", "readyReplicas")}`);
  await expect(ready).toHaveText(/^1/); // the snapshot

  // The user starts editing spec.replicas WHILE the status is still churning underneath them.
  const replicas = page.getByTestId(`input:${path("spec", "replicas")}`);
  await replicas.fill("4");

  // ...and now the controller's status update lands.
  await expect(ready).toHaveText(/^2/);
  await expect(ready).toHaveAttribute("data-flashed", "true"); // read-only is NOT ignored

  // The edit survived the event untouched (I-NONINTERFERE), and it is the ONLY thing dirty.
  await expect(replicas).toHaveValue("4");
  await expect(page.getByTestId("badge:dirty")).toHaveCount(1);
  await expect(page.getByTestId("badge:conflict")).toHaveCount(0);

  // status is not in the patch. It cannot be.
  await expect(page.getByTestId("patch")).toHaveText(JSON.stringify({ spec: { replicas: 4 } }, null, 2));
});

test("a conflict is raised, the typing is never overwritten, and it clears on convergence", async ({ page }) => {
  // The server moves data.log-level to `warn` while the user is typing `debug` (a real conflict), and
  // then a colleague makes the same change the user did — so the server ARRIVES at `debug` and the
  // conflict must clear itself. A cached dirty flag gets the second half wrong and stays red forever.
  await page.goto("/?fixture=conflict-and-converge&pace=700ms");

  const logLevel = page.getByTestId(`input:${path("data", "log-level")}`);
  await expect(logLevel).toHaveValue("info");
  await logLevel.fill("debug");

  // The server says `warn`. Keep what the user typed, and TELL them what the cluster says.
  await expect(page.getByTestId("badge:conflict")).toBeVisible({ timeout: 10_000 });
  await expect(page.getByTestId("badge:conflict")).toHaveText(/server says "warn"/);
  await expect(logLevel).toHaveValue("debug", { timeout: 1 }); // never silently overwritten

  // The server converges on `debug`. The conflict clears, and the field stops being dirty — because
  // dirtiness is DERIVED (draft == server now), not remembered.
  await expect(page.getByTestId("badge:conflict")).toHaveCount(0, { timeout: 10_000 });
  await expect(page.getByTestId("badge:dirty")).toHaveCount(0);
  await expect(logLevel).toHaveValue("debug");
  await expect(page.getByTestId("patch")).toHaveText("null"); // nothing left to save
});

test("an unrelated server change never disturbs the field you are editing", async ({ page }) => {
  // R-THREEWAY. The server bumps `replicas`; nobody touched `log-level`. Compare the incoming object
  // to the DRAFT instead of to the previous SERVER object and this false-conflicts on every heartbeat.
  await page.goto("/?fixture=edit-vs-unrelated-change&pace=700ms");

  const logLevel = page.getByTestId(`input:${path("data", "log-level")}`);
  await expect(logLevel).toHaveValue("info");
  await logLevel.fill("debug");

  // The unrelated key follows the server...
  await expect(page.getByTestId(`input:${path("data", "replicas")}`)).toHaveValue("5", { timeout: 10_000 });
  // ...and the edit is untouched, dirty, and NOT in conflict.
  await expect(logLevel).toHaveValue("debug");
  await expect(page.getByTestId("badge:conflict")).toHaveCount(0);
  await expect(page.getByTestId("patch")).toHaveText(JSON.stringify({ data: { "log-level": "debug" } }, null, 2));
});

test("a redacted Secret value is shown as a mask, cannot be edited, and never reaches the patch", async ({ page }) => {
  // The rule that makes a Secret safe to display at all: a value you never SAW cannot round-trip over
  // the real one.
  await page.goto("/?fixture=secret-redaction&pace=0ms");
  await expect(page.locator("#status-line")).toHaveText(/synced/);

  const token = page.getByTestId(`value:${path("data", "token")}`);
  await expect(token).toContainText("**REDACTED**");

  // Read-only means there is no input to type into. Not "an input we ignore" — no input.
  await expect(page.getByTestId(`input:${path("data", "token")}`)).toHaveCount(0);
  await expect(page.getByTestId("patch")).toHaveText("null");
});

test("a named object that does not exist renders as empty, not as a ghost and not as an error", async ({ page }) => {
  // reset, synced — and nothing else. The fixture that kills the "named scopes may skip the snapshot"
  // optimization: skip it, and a delete-while-disconnected leaves the object on screen forever.
  await page.goto("/?fixture=named-object-absent&pace=0ms");
  await expect(page.locator("#status-line")).toHaveText(/synced/);
  await expect(page.getByTestId("editable-body")).toBeEmpty();
  await expect(page.getByTestId("patch")).toHaveText("null");
});
