#!/usr/bin/env bash
# Start the comparison host against the kubeconfig's cluster, run measure.ts against it, stop it.
#
#   measure.sh [measure.ts options]
#       The matrix, against this tree's built library (task build-client).
#
#   measure.sh --baseline-ref REF [measure.ts options]
#       Native before/after. Builds the library as it is at REF (default for the task: main) in a
#       scratch directory, measures the native source against it, then against this tree's library,
#       with identical options, and writes a before/after table. Pass the commit BEFORE the change
#       under test: once main contains the change, `main` is no longer a baseline.
#
# Results land in examples/comparison/results/ (or --out). The host listens on $COMPARE_ADDR
# (default 127.0.0.1:8110) and deletes its scratch namespace when it stops.
set -euo pipefail

repo="$(cd "$(dirname "$0")/../.." && pwd)"
addr="${COMPARE_ADDR:-127.0.0.1:8110}"
ref=""
if [[ "${1:-}" == "--baseline-ref" ]]; then
  ref="${2:?--baseline-ref needs a git ref}"
  shift 2
fi

scratch="$(mktemp -d -t krm-compare-XXXX)"
bin="$scratch/compare"
log="$scratch/host.log"

# BUILD, then run the binary — not `go run`, whose child would outlive the pid we know about and keep
# the port. Its output goes to a file so a background process never holds our stdout open.
(cd "$repo/gateway/kube" && go build -o "$bin" ./examples/comparison/cmd/compare)
"$bin" --addr "$addr" --tls-addr "" --dist "$repo/packages/krm-stream/dist" --page "$repo/examples/comparison" >"$log" 2>&1 &
pid=$!
cleanup() {
  # SIGINT, then wait: the host deletes its namespace on the way out.
  kill -INT "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  rm -rf "$scratch"
}
trap cleanup EXIT

for _ in $(seq 240); do
  curl -sf "http://$addr/healthz" >/dev/null && break
  kill -0 "$pid" 2>/dev/null || break
  sleep 0.5
done
curl -sf "http://$addr/healthz" >/dev/null || { echo "the comparison host never came up:"; cat "$log"; exit 1; }
head -1 "$log"

measure=(node "$repo/examples/comparison/measure.ts" --host "http://$addr")
current="current-$(git -C "$repo" rev-parse --short HEAD)$(git -C "$repo" diff --quiet HEAD -- packages/krm-stream/src || echo -dirty)"

if [[ -z "$ref" ]]; then
  "${measure[@]}" --label "$current" "$@" >/dev/null
  exit
fi

# The baseline library: REF's packages/krm-stream, built in the scratch directory. The current
# tree's node_modules is reused when REF's lockfile is identical, so no download is needed.
sha="$(git -C "$repo" rev-parse --short "$ref^{commit}")"
pkg="$scratch/baseline/packages/krm-stream"
mkdir -p "$scratch/baseline"
git -C "$repo" archive "$sha" packages/krm-stream | tar -x -C "$scratch/baseline"
if cmp -s "$pkg/package-lock.json" "$repo/packages/krm-stream/package-lock.json"; then
  ln -s "$repo/packages/krm-stream/node_modules" "$pkg/node_modules"
else
  (cd "$pkg" && npm ci --no-audit --no-fund)
fi
(cd "$pkg" && npm run --silent build)
echo "baseline library: $ref ($sha) → $pkg/dist"

status=0
before="$("${measure[@]}" --lib "$pkg/dist" --sources native --label "baseline-$sha" "$@" | tail -1)" || status=$?
after="$("${measure[@]}" --sources native --label "$current" "$@" | tail -1)" || status=$?
node "$repo/examples/comparison/measure.ts" --compare "$before" "$after" --out "${after%.json}-vs-baseline-$sha.md"
echo "before/after table: ${after%.json}-vs-baseline-$sha.md"
exit "$status"
