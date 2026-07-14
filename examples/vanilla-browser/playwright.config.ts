import { defineConfig } from "@playwright/test";
import type { Entry } from "./tests/fixtures.ts";

// The browser rung of the e2e ladder — and it needs no cluster.
//
// One process serves everything, from ONE origin: the replay gateway serves the scripted corpus as
// real SSE, the built ESM at /krm-stream/, and this example at /. Same-origin is not a convenience —
// it is the deployment the protocol is specified around (spec §7: native EventSource cannot send an
// Authorization header, so the v1 baseline is same-origin session cookies). A demo served from a
// second port would need CORS, and would then be proving something we do not ship.

export default defineConfig<{ entry: Entry }>({
  testDir: "./tests",
  fullyParallel: true,
  forbidOnly: !!process.env["CI"],
  retries: process.env["CI"] ? 1 : 0,
  reporter: process.env["CI"] ? "list" : [["list"]],
  use: {
    baseURL: "http://127.0.0.1:8100",
    trace: "retain-on-failure",
  },
  // The same suite, twice, against the two published entry points. `./bundle` is not a convenience
  // build that can be allowed to rot: it is what a host with no node_modules actually vendors, so it
  // gets the same real browser, the same real EventSource, and the same assertions as index.js. If a
  // flattening step ever dropped an export or a polyfill crept in, the bundle project goes red on its
  // own while the module project stays green — which is exactly the signal you want.
  projects: [
    { name: "chromium", use: { browserName: "chromium", entry: "index" } },
    { name: "chromium-bundle", use: { browserName: "chromium", entry: "bundle" } },
  ],

  // Build the library first — `dist/` is what the browser imports, unbundled, and a stale dist is a
  // test of last week's library.
  //
  // `npm run build` rather than a hand-copied `tsc` line: the build is now two steps (tsc, then the
  // esbuild flatten into dist/krm-stream.js), and a browser test that builds them differently from a
  // release is testing a file nobody ships. It resolves binaries from node_modules/.bin, so it never
  // reaches the network — which is the property that matters here. Bare `npx tsc` in a directory with
  // no node_modules does NOT fail: it goes to the REGISTRY, downloads whatever is published under the
  // name `tsc` (a squatter package, not TypeScript), and runs it. That is what this job did on its
  // first-ever CI run. Run `task e2e-browser`, which installs the library's devDependencies first.
  webServer: {
    command:
      "cd ../../packages/krm-stream && npm run --silent build && " +
      "cd ../../gateway && go run ./cmd/replay " +
      "--addr 127.0.0.1:8100 --corpus ../conformance " +
      "--static ../examples/vanilla-browser --dist ../packages/krm-stream/dist",
    url: "http://127.0.0.1:8100/healthz",
    reuseExistingServer: !process.env["CI"],
    timeout: 120_000,
    stdout: "pipe",
    stderr: "pipe",
  },
});
