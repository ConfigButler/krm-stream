import { defineConfig } from "@playwright/test";

// The browser rung of the e2e ladder — and it needs no cluster.
//
// One process serves everything, from ONE origin: the replay gateway serves the scripted corpus as
// real SSE, the built ESM at /krm-stream/, and this example at /. Same-origin is not a convenience —
// it is the deployment the protocol is specified around (spec §7: native EventSource cannot send an
// Authorization header, so the v1 baseline is same-origin session cookies). A demo served from a
// second port would need CORS, and would then be proving something we do not ship.

export default defineConfig({
  testDir: "./tests",
  fullyParallel: true,
  forbidOnly: !!process.env["CI"],
  retries: process.env["CI"] ? 1 : 0,
  reporter: process.env["CI"] ? "list" : [["list"]],
  use: {
    baseURL: "http://127.0.0.1:8100",
    trace: "retain-on-failure",
  },
  projects: [{ name: "chromium", use: { browserName: "chromium" } }],

  // Build the library first — `dist/` is what the browser imports, unbundled, and a stale dist is a
  // test of last week's library.
  webServer: {
    command:
      "cd ../../packages/krm-stream && npx tsc && " +
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
