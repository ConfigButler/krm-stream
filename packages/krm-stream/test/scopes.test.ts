import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { resourceStreamURL, type ScopeQuery } from "../src/url.ts";

// The client's half of conformance/scopes.yaml — the REQUEST half of the wire.
//
// The Go suite reads the same table and asserts its parser ACCEPTS `canonical` and yields `scope`.
// This one asserts the client PRODUCES `canonical` from that same `scope`. Neither end can drift
// without the other's suite going red, which is the entire reason the table is shared rather than
// duplicated — the failure it prevents (the two ends of one repo quietly disagreeing about a URL)
// is one no amount of per-side unit testing can catch.

interface ScopeCase {
  id: string;
  because?: string;
  query: string;
  canonical?: string;
  scope?: ScopeQuery;
  code?: string;
}

const cases: ScopeCase[] = JSON.parse(
  readFileSync(new URL("../../../conformance/gen/scopes.json", import.meta.url), "utf8"),
);

test("the scope corpus is present", () => {
  assert.ok(cases.length > 0, "no scope cases — run `task fixtures`");
});

for (const c of cases) {
  if (c.code) continue; // a rejected scope has no canonical form; refusing it is the gateway's job

  test(`resourceStreamURL builds the canonical query: ${c.id}`, () => {
    assert.ok(c.scope, `${c.id} has no scope`);
    assert.ok(c.canonical, `${c.id} has no canonical query`);

    const url = resourceStreamURL("/resource-stream/v1", c.scope);
    const query = url.slice(url.indexOf("?") + 1);

    // Exact string equality, field order included. A URL that varies by key order is one a cache and
    // a log aggregator each see as two different URLs.
    assert.equal(query, c.canonical, c.because ?? c.id);
  });
}

test("the base URL's existing query is preserved, not clobbered", () => {
  const url = resourceStreamURL("/stream?tenant=acme", { version: "v1", resource: "configmaps" });
  assert.equal(url, "/stream?tenant=acme&version=v1&resource=configmaps");
});

test("a scope with no resource or no version is refused at the source", () => {
  // The gateway refuses these too (the corpus asserts it) — but failing HERE means a typo is a
  // stack trace in the developer's console, not a terminal error event they have to go and read.
  assert.throws(() => resourceStreamURL("/s", { version: "v1" } as ScopeQuery), /resource/);
  assert.throws(() => resourceStreamURL("/s", { resource: "configmaps" } as ScopeQuery), /version/);
});

test("there is nowhere to put an API-server address", () => {
  // Not an assertion about a value — an assertion about the TYPE. `ScopeQuery` has no field for a
  // server, endpoint or credential, which is spec §8 made structural instead of merely promised.
  const scope: ScopeQuery = { version: "v1", resource: "secrets", namespace: "app" };
  assert.equal(resourceStreamURL("/s", scope), "/s?version=v1&resource=secrets&namespace=app");
  assert.ok(!("server" in scope));
});
