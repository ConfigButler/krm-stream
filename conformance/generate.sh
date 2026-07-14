#!/usr/bin/env bash
# Build conformance/gen/*.json from the YAML sources.
#
# YAML is the source of truth (a human reasons about a Kubernetes object, not about escaped JSON);
# JSON is what two languages parse with zero dependencies. CI fails if gen/ is stale.
#
# Two different tools are called `yq` in the wild — mikefarah's Go one (`yq -o=json`) and the Python
# jq-wrapper (`yq .`, JSON by default). The devcontainer ships mikefarah's; a contributor's laptop
# may have either, and being fussy about it is a worse first-five-minutes than just coping.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
mkdir -p "${here}/gen"

command -v yq >/dev/null || { echo "conformance: yq is required (see .devcontainer/Dockerfile)" >&2; exit 1; }
command -v jq >/dev/null || { echo "conformance: jq is required" >&2; exit 1; }

if yq --version 2>&1 | grep -qi mikefarah; then
  to_json() { yq -o=json '.' "$1"; }
else
  to_json() { yq '.' "$1"; }   # python-yq emits JSON already
fi

# bodies.json — a map of  <file stem>  ->  the KRM object.
for f in "${here}"/bodies/*.yaml; do
  id="$(basename "${f}" .yaml)"
  to_json "${f}" | jq --arg id "${id}" '{($id): .}'
done | jq -s 'add' > "${here}/gen/bodies.json"

# fixtures.json — an array of scenarios, sorted by id so the output is deterministic.
for f in "${here}"/fixtures/*.yaml; do
  to_json "${f}"
done | jq -s 'sort_by(.id)' > "${here}/gen/fixtures.json"

# scopes.json — the REQUEST half: how a scope is encoded in a URL. Read by BOTH suites, which is the
# whole point: the client builds `canonical`, the gateway parses it back, and neither can drift.
to_json "${here}/scopes.yaml" | jq 'sort_by(.id)' > "${here}/gen/scopes.json"

echo "conformance: $(jq 'length' "${here}/gen/bodies.json") bodies, $(jq 'length' "${here}/gen/fixtures.json") fixtures, $(jq 'length' "${here}/gen/scopes.json") scopes -> gen/"
