// Compile the native editor and the editor recipes the page reuses into browser-loadable ESM in
// dist/. They stay TypeScript in the repository, where the client suite runs them; this only strips
// types. Their library import, a relative path to the TypeScript source, becomes
// "@configbutler/krm-stream", exactly as a host copying them would write it. The page's import map
// points that name at one built entry point, so the page and the editor share one copy of the
// library: index.js or the single-file bundle.
//
// Run with `task build-native-editor`. esbuild is the client package's own devDependency.

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
  entryPoints: ["editor.ts", "../editor-recipes/keepLocal.ts", "../editor-recipes/recoveryCopy.ts"].map((path) => ({
    in: new URL(path, import.meta.url).pathname,
    out: path.replace(/^.*\//, "").replace(/\.ts$/, ""),
  })),
  outdir: new URL("dist", import.meta.url).pathname,
  bundle: true,
  format: "esm",
  target: "es2022",
  legalComments: "none",
  plugins: [library],
  logLevel: "warning",
});
