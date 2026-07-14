// One suite, two entry points.
//
// The library publishes the per-module build (dist/index.js, which imports ./store.js, which imports
// ./merge.js…) AND a flattened single file (dist/krm-stream.js) for hosts that vendor the library by
// copying it and have no bundler to resolve that graph. Both are public API. Only one of them was
// ever loaded in a real browser.
//
// So `entry` is a project option rather than a parameter of any one test: playwright.config.ts runs
// this entire spec twice, once per entry point, and `visit` puts the choice in the URL the demo page
// reads. A test that forgets to thread it through does not silently test index.js twice — it cannot
// navigate at all.

import { test as base } from "@playwright/test";

export type Entry = "index" | "bundle";

export const test = base.extend<{
  entry: Entry;
  /** Open the demo page for a fixture, on whichever entry point this project is exercising. */
  visit: (query: string) => Promise<void>;
}>({
  entry: ["index", { option: true }],
  visit: async ({ page, entry }, use) => {
    await use(async (query) => {
      await page.goto(`/?entry=${entry}&${query}`);
    });
  },
});

export { expect } from "@playwright/test";
