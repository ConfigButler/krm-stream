#!/usr/bin/env bash
# Runs on every container start (not just create). Keep it cheap.
set -euo pipefail

workspace_dir="${1:-${containerWorkspaceFolder:-$(pwd)}}"

# The SSH agent socket is a fresh path on every start, so the signing config has to be
# re-pointed each time or `git commit` fails with a signing error.
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh" \
  || echo "[post-start] warning: SSH commit signing not configured — commits will be unsigned"
