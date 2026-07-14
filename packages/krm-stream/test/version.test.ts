import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { PROTOCOL_VERSION, VERSION } from "../src/version.ts";

// A version stamp that can go stale is worse than none: it is a claim, and a false one. These two
// tests are what make it a fact instead.

test("VERSION matches package.json — a vendored build states its own provenance truthfully", () => {
  const pkg = JSON.parse(readFileSync(new URL("../package.json", import.meta.url), "utf8"));
  assert.equal(
    VERSION,
    pkg.version,
    "src/version.ts has drifted from package.json — the stamp a host asserts against would be a lie",
  );
});

test("PROTOCOL_VERSION matches the Go constant that DEFINES it", () => {
  // Published by the gateway itself (`task fixtures` → conformance/gen/protocol.json), not copied by
  // hand. This is the seam the reviewer named: a vendored client and the gateway it was built
  // against, drifting silently. Neither half can bump the protocol without the other's suite going
  // red.
  const published = JSON.parse(
    readFileSync(new URL("../../../conformance/gen/protocol.json", import.meta.url), "utf8"),
  );
  assert.equal(
    PROTOCOL_VERSION,
    published.protocolVersion,
    "the client and the gateway disagree about which protocol they speak",
  );
});
