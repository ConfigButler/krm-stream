// The version stamp, and it exists for one failure that is otherwise silent.
//
// This library is publishable to npm, but it is also VENDORED — copied, as built ESM, into a host
// that serves it to a browser (that is what the gateway's `--dist` flag is for). A vendored asset
// drifts. It keeps working, too: an old client and a new gateway agree about every event they both
// still understand, right up until the wire changes, and then the failure lands in someone's browser
// rather than in anyone's test suite.
//
// So the bytes carry their own provenance, and a host can assert it:
//
//	import { VERSION, PROTOCOL_VERSION } from "krm-stream";
//	// in the host's own test suite:
//	assert.equal(PROTOCOL_VERSION, protocolVersionFromMyGoMod);
//
// Neither number is hand-maintained in the sense that matters: `task test` fails if VERSION drifts
// from package.json, and if PROTOCOL_VERSION drifts from the Go constant that DEFINES it (the gateway
// publishes it into conformance/gen/protocol.json, and the suite reads it back). Neither half of this
// repo can bump the protocol alone.
//
// VERSION is written by release-please, which finds it by the annotation on the line itself (see
// release-please-config.json). Do not edit it by hand: the release PR bumps package.json and this
// line together, and version.test.ts fails if they ever disagree.

/** The npm package version of this build. Assert it against the copy you vendored. */
export const VERSION = "0.0.0"; // x-release-please-version

/** The wire protocol this build speaks (spec/v1.md). The gateway sends it as `X-KRM-Stream-Protocol`. */
export const PROTOCOL_VERSION = 1;
