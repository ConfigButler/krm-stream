# sample-apiserver — a real aggregated API, for the facts that need one

Kubernetes' own [sample-apiserver](https://github.com/kubernetes/sample-apiserver) (`wardle.example.com`,
kinds `Flunder` and `Fischer`), installed as a genuine **aggregated API** behind an `APIService`.

It is here for one question, and it is a question no amount of reading answers: **an aggregated API
server is the one upstream Kubernetes' conformance rules do not cover** (see
[docs/facts/kubernetes-api-concepts.md](../../../docs/facts/kubernetes-api-concepts.md)), so it is the
only place `Gateway.OrderingLenient` could ever be needed — and the only place the gateway's
`sendInitialEvents` assumption might not hold.

The `resourceversion-bignum` and `resourceversion-unorderable` fixtures already model a `Flunder`.
This makes it real.

These manifests are derived from **upstream's `artifacts/example/`**, not from any ConfigButler repo —
this library depends on nothing of ours, and that rule includes YAML.

Two things worth knowing:

- **The etcd sidecar is not optional.** `sample-apiserver`'s `--etcd-servers` will not take an empty
  value; it needs a real store. Upstream's own example uses the sidecar, and the data does not need to
  outlive the pod.
- **The image tag lags.** Only `1.33.8` is published; there is no `1.35`/`1.36` tag. That is fine — an
  `APIService` aggregates over HTTP, and the skew is precisely the sort of thing a real deployment has.
