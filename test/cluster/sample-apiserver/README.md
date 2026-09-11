# sample-apiserver — a real aggregated API, for the facts that need one

Kubernetes' own [sample-apiserver](https://github.com/kubernetes/sample-apiserver) (`wardle.example.com`,
kinds `Flunder` and `Fischer`), installed as a genuine **aggregated API** behind an `APIService`.

This exercises the list-then-watch fallback against an aggregated API that can reject streaming
lists. See the [recorded observations](../../../docs/facts/observed-v1.36.2+k3s1.md) for the tested
server version and results. The `resourceversion-*` fixtures separately cover arbitrary-size and
unorderable versions; a real sample-apiserver run does not establish either case.

The manifests are derived from upstream's `artifacts/example/`.

Two things worth knowing:

- **The etcd sidecar is not optional.** `sample-apiserver`'s `--etcd-servers` will not take an empty
  value; it needs a real store. Upstream's own example uses the sidecar, and the data does not need to
  outlive the pod.
- **The manifest pins `1.33.8`.** This aggregated server has its own feature gates and runs behind an
  `APIService`; its behavior is measured separately from the cluster API server.
