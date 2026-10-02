// Rehearse a release pull request: build the Go adapter the way it will be built in one.
//
// Release Please rewrites every line marked `x-release-please-version` to the version being released,
// and that version's tag does not exist until the release PR merges. Builds that pass on main can
// therefore fail on the release PR (0.6.0 did: workspace mode still read gateway@v0.6.0's go.mod).
// This applies Release Please's own version regex to the marked lines, with a version that can never
// be tagged, vets the adapter, and restores the files.
//
// It also refuses drift: go.work's replacement must name the version gateway/kube/go.mod requires.

import { execFileSync } from "node:child_process";
import { readFileSync, writeFileSync } from "node:fs";

// Verbatim from release-please src/updaters/generic.ts.
const VERSION_REGEX = /(?<major>\d+)\.(?<minor>\d+)\.(?<patch>\d+)(-(?<preRelease>[\w.]+))?(\+(?<build>[-\w.]+))?/;
const MARKER = "x-release-please-version";
// A v0 version (the module path has no /vN suffix) that will never be tagged.
const REHEARSED = "0.999.0";
const CORE = "github.com/ConfigButler/krm-stream/gateway v";

const files = ["gateway/kube/go.mod", "go.work"];
const originals = new Map(files.map((f) => [f, readFileSync(f, "utf8")]));

const coreVersion = (file) => {
  const line = originals
    .get(file)
    .split("\n")
    .find((l) => l.includes(CORE) && l.includes(MARKER));
  if (!line) throw new Error(`${file}: no ${MARKER} line for the core module`);
  return line.slice(line.indexOf(CORE) + CORE.length).split(/\s/)[0];
};

const required = coreVersion("gateway/kube/go.mod");
const replaced = coreVersion("go.work");
if (required !== replaced) {
  console.error(
    `::error file=go.work::go.work replaces core v${replaced}, but gateway/kube/go.mod requires v${required}. ` +
      "They must name the same version; Release Please moves both.",
  );
  process.exit(1);
}

try {
  for (const [file, text] of originals) {
    const rehearsed = text
      .split("\n")
      .map((line) => (line.includes(MARKER) ? line.replace(VERSION_REGEX, REHEARSED) : line))
      .join("\n");
    writeFileSync(file, rehearsed);
  }
  if (!readFileSync("gateway/kube/go.mod", "utf8").includes(`${CORE}${REHEARSED}`)) {
    throw new Error("the rehearsal did not rewrite the adapter's core requirement");
  }
  const run = (...args) => execFileSync("go", args, { cwd: "gateway/kube", stdio: "inherit" });
  try {
    run("vet", "-tags", "e2e", "./...");
    run("build", "./...");
  } catch {
    console.error(`::error::the adapter does not build as a release PR for v${REHEARSED} would build it (see go's output above)`);
    process.exitCode = 1;
  }
  if (!process.exitCode) console.log(`the adapter builds as a release PR for v${REHEARSED} would build it`);
} finally {
  for (const [file, text] of originals) writeFileSync(file, text);
}
