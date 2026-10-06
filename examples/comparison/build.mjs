// Compile the comparison page's controller (page.ts) — with the native editor, the conditional
// editor and the keep-local recipe it reuses — into one browser-loadable module, dist/page.js. Types
// are stripped; the library import, a relative path to the TypeScript source, becomes
// "@configbutler/krm-stream", which the page's import map points at one built entry point. The page,
// the editors and the recipe therefore share one copy of the library: index.js or the bundle.
//
// Run with `task build-compare-page`. esbuild is the client package's own devDependency.

import { createRequire } from "node:module";

const { build } = createRequire(new URL("../../packages/krm-stream/package.json", import.meta.url))("esbuild");

const library = {
  name: "published-library",
  setup(build) {
    build.onResolve({ filter: /\/packages\/krm-stream\/src\/index\.ts$/ }, () => ({
      path: "@configbutler/krm-stream",
      external: true,
    }));
  },
};

await build({
  entryPoints: [new URL("page.ts", import.meta.url).pathname],
  outfile: new URL("dist/page.js", import.meta.url).pathname,
  bundle: true,
  format: "esm",
  target: "es2022",
  legalComments: "none",
  plugins: [library],
  logLevel: "warning",
});
