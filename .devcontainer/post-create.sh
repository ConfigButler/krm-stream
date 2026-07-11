#!/usr/bin/env bash
# Runs once, when the container is created. Keep it fast and idempotent.
set -euo pipefail

workspace="${1:-$PWD}"
cd "${workspace}"

echo "==> git identity"
[[ -n "${GIT_USER_NAME:-}" ]] && git config --global user.name "${GIT_USER_NAME}"
[[ -n "${GIT_USER_EMAIL:-}" ]] && git config --global user.email "${GIT_USER_EMAIL}"
git config --global --add safe.directory "${workspace}"

echo "==> go modules (gateway)"
(cd gateway && go mod download)

echo "==> node modules (client)"
(cd packages/krm-stream && npm install --no-audit --no-fund)

echo "==> conformance fixtures"
task fixtures

cat <<'EOF'

  krm-stream is ready.

    task           list every task
    task test      both suites, against the shared conformance fixtures
    task lint      go vet + golangci-lint + tsc --noEmit
    task fixtures  rebuild conformance/gen/*.json from the YAML sources

  The contract lives in spec/v1.md; the fixtures that enforce it live in conformance/.

EOF
