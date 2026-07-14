#!/usr/bin/env bash
# Runs once, when the container is created. Idempotent — a rebuild re-runs it.
set -euo pipefail

log() { echo "[post-create] $*"; }
fail() { echo "[post-create] ERROR: $*" >&2; exit 1; }

workspace_dir="${1:-${containerWorkspaceFolder:-$(pwd)}}"
log "workspace: ${workspace_dir}"

# ---------------------------------------------------------------- git identity --
git_name="$(git config --get user.name || true)"
git_email="$(git config --get user.email || true)"
[ -z "${git_name}" ] && git_name="${GIT_USER_NAME:-}"
[ -z "${git_email}" ] && git_email="${GIT_USER_EMAIL:-}"
if [ -z "${git_name}" ] || [ -z "${git_email}" ]; then
  fail "Missing Git identity. Set user.name/user.email in Git, or pass GIT_USER_NAME/GIT_USER_EMAIL to the devcontainer."
fi
git config --global --get user.name  >/dev/null 2>&1 || git config --global user.name  "${git_name}"
git config --global --get user.email >/dev/null 2>&1 || git config --global user.email "${git_email}"
git config --global --add safe.directory "${workspace_dir}"

# Non-fatal, unlike gitops-api's: this is a public repo, and a contributor without a forwarded
# SSH agent should still get a working container — they just get unsigned commits.
log "refreshing Git SSH signing configuration"
bash "${workspace_dir}/.devcontainer/sync-signing-key.sh" \
  || log "warning: SSH commit signing not configured (no agent / no key?) — commits will be unsigned"

# ------------------------------------------------------- home ownership (READ ME) --
# Every named volume mounted under /home/vscode (~/.claude, ~/.codex, ~/.config/gh, ~/.kube, the
# Go caches, persisted-home) is created by Docker as ROOT-owned, because the image has no such
# directory to inherit ownership from. Without this chown, the `vscode` user cannot write to
# any of them — and the symptom is baffling: `claude` and `codex` cannot log in, `gh auth login`
# cannot store a token, and kubectl cannot write a kubeconfig. Same fix as gitops-api's.
log "ensuring cache dirs exist, then fixing ownership under /home/vscode"
sudo mkdir -p \
  /home/vscode/.cache/go-build \
  /home/vscode/.cache/golangci-lint \
  /home/vscode/.claude \
  /home/vscode/.codex \
  /home/vscode/.config/gh \
  /home/vscode/.kube \
  /home/vscode/persisted-home
sudo chown -R vscode:vscode /home/vscode || true
[ -d "${workspace_dir}" ] && sudo chown -R vscode:vscode "${workspace_dir}" || true

# Claude Code stores its credentials in ~/.claude.json — a FILE at the home root, NOT inside
# ~/.claude/ — so mounting ~/.claude alone does not persist a login. Symlink it into the
# persisted-home volume, so a rebuild (which editing devcontainer.json triggers) doesn't sign
# you out. Same trick as gitops-api's.
touch /home/vscode/persisted-home/.claude.json
rm -f /home/vscode/.claude.json
ln -s /home/vscode/persisted-home/.claude.json /home/vscode/.claude.json

# --------------------------------------------------------------------- the repo --
# The Go module is nested (gateway/go.mod) so `go get` works without dragging in the TS side.
log "go modules (the gateway)"
(cd "${workspace_dir}/gateway" && go mod download)

log "node modules (the TS helper)"
(cd "${workspace_dir}/packages/krm-stream" && npm install --no-audit --no-fund)

log "conformance fixtures"
(cd "${workspace_dir}" && task fixtures)

cat <<'EOF'

  krm-stream is ready.

    task           list every task
    task test      both suites, against the shared conformance fixtures
    task lint      go vet + golangci-lint + tsc --noEmit
    task fixtures  rebuild conformance/gen/*.json from the YAML sources

  The contract lives in spec/v1.md; the fixtures that enforce it live in conformance/.

EOF
log "done"
