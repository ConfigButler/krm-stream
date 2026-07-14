# krm-stream

**A live, faithful window onto Kubernetes resources, in the browser.**

This npm package is the **browser client**: it reads the stream a `krm-stream` gateway serves, keeps
a store of complete KRM objects in step with the cluster, three-way merges your local edits against
the live object, and builds a merge patch. It has **zero runtime dependencies** and ships as plain
ESM a browser imports with no bundler.

It is one half of a pair. The other half — **the Go gateway that produces the stream** — is the
product, and this client is the helper you would otherwise have had to write. Neither half is much
use without the other, so the documentation lives in one place rather than being half-told here:

### 📖 **[Read the documentation → github.com/ConfigButler/krm-stream](https://github.com/ConfigButler/krm-stream)**

```bash
npm install @configbutler/krm-stream
```

The unscoped `krm-stream` package is kept as a tiny compatibility forwarder, but new code should
import from `@configbutler/krm-stream` so the package name matches the GitHub owner.

Licensed under Apache-2.0.
