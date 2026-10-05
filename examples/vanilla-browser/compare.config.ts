import { defineConfig } from "@playwright/test";
import type { Entry } from "./tests/fixtures.ts";

// The comparison page (examples/comparison) in Chromium against the REAL comparison host and a real
// API server, on both entry points. Not part of `task e2e-browser`, which needs no cluster: run it with
// `task compare-browser`, which builds the host binary and passes it in COMPARE_BIN.
//
// One host, one scratch namespace and the same Widgets for every test, so the tests run one at a time.
// The host listens on 8120/8121, apart from `task compare` (8110/8111) and the browser suite (8100).

const bin = process.env["COMPARE_BIN"];
if (!bin) throw new Error("COMPARE_BIN must name a built comparison host; run `task compare-browser`");

export default defineConfig<{ entry: Entry }>({
  testDir: "./compare",
  workers: 1,
  fullyParallel: false,
  reporter: [["list"]],
  use: {
    // HTTP/2 over TLS: the page holds six streams, which would take every HTTP/1.1 connection a
    // browser allows one origin. The certificate is the host's own, self-signed at startup.
    baseURL: "https://127.0.0.1:8121",
    ignoreHTTPSErrors: true,
    trace: "retain-on-failure",
  },
  projects: [
    { name: "chromium", use: { browserName: "chromium", entry: "index" } },
    { name: "chromium-bundle", use: { browserName: "chromium", entry: "bundle" } },
  ],
  webServer: {
    command: `${bin} --addr 127.0.0.1:8120 --tls-addr 127.0.0.1:8121 --dist ../../packages/krm-stream/dist --page ../comparison`,
    url: "http://127.0.0.1:8120/healthz",
    reuseExistingServer: false,
    timeout: 180_000,
    // SIGINT, so the host deletes its scratch namespace on the way out.
    gracefulShutdown: { signal: "SIGINT", timeout: 30_000 },
    stdout: "pipe",
    stderr: "pipe",
  },
});
