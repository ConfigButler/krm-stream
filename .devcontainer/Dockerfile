# Devcontainer for krm-stream — a two-language repo (Go gateway + TypeScript client).
#
# Deliberately slimmer than gitops-api's: no Flux, no kcp, no cert-manager, no
# cloudflared. This repo knows nothing about ConfigButler — it knows KRM. What it
# needs is a Go toolchain, a Node toolchain, Task, and (for proving the gateway
# against a REAL API server rather than only a fake watch) k3d + kubectl.
#
# Stage 1 (ci): everything the test suites and CI need.
# Stage 2 (dev): editor tooling + the non-root `vscode` user.

# Base pinned by digest — the SAME image gitops-api builds on, so this repo's Go behaves
# identically to the one that consumes it.
FROM golang:1.26.5-bookworm@sha256:18aedc16aa19b3fd7ded7245fc14b109e054d65d22ed53c355c899582bbb2113 AS ci

ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    git curl ca-certificates vim less jq sudo acl gnupg unzip openssh-client \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
       -o /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && chmod a+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
       > /etc/apt/sources.list.d/github-cli.list \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
       > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get -y install --no-install-recommends gh docker-ce-cli docker-buildx-plugin \
    && apt-get autoremove -y && apt-get clean -y && rm -rf /var/lib/apt/lists/*

# Tool versions — centralized.
# GOLANGCI_LINT_VERSION -> https://github.com/golangci/golangci-lint/releases
# TASK_VERSION          -> https://github.com/go-task/task/releases
# YQ_VERSION            -> https://github.com/mikefarah/yq/releases  (builds conformance/gen/*.json)
# KUBECTL_VERSION / K3D_VERSION — kept in lockstep with gitops-api, so a cluster stood up
#   here behaves identically to the one the gateway will really run against.
# NODE_MAJOR            -> the current Node LTS line. 22 runs `.ts` under `node --test`
#   natively (type stripping), which is why this repo's client suite needs ZERO test deps.
ENV PATH="/go/bin:/usr/local/go/bin:${PATH}" \
    GOLANGCI_LINT_VERSION=v2.12.2 \
    TASK_VERSION=v3.51.1 \
    YQ_VERSION=v4.48.1 \
    KUBECTL_VERSION=v1.36.2 \
    K3D_VERSION=v5.9.0 \
    NODE_MAJOR=22

RUN test "$(dpkg --print-architecture)" = "amd64" \
    || (echo "This devcontainer currently supports amd64 only." && exit 1)

RUN curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

RUN curl -fsSL "https://github.com/go-task/task/releases/download/${TASK_VERSION}/task_linux_amd64.tar.gz" \
    | tar -xzO task > /usr/local/bin/task && chmod +x /usr/local/bin/task

RUN curl -fsSL -o /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64" \
    && chmod +x /usr/local/bin/yq

RUN curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x /usr/local/bin/kubectl

RUN curl -fsSL -o /usr/local/bin/k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-amd64" \
    && chmod +x /usr/local/bin/k3d

# Node.js — the client's test runner AND its build (tsc). Nothing else needs it.
RUN curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
       | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
    && chmod a+r /etc/apt/keyrings/nodesource.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
       > /etc/apt/sources.list.d/nodesource.list \
    && apt-get update \
    && apt-get -y install --no-install-recommends nodejs \
    && apt-get autoremove -y && apt-get clean -y && rm -rf /var/lib/apt/lists/*

WORKDIR /workspaces

# Shared Go dir access for both root (build) and vscode (dev).
RUN groupadd --gid 2000 godev \
    && chgrp -R godev /go && chmod -R 2775 /go \
    && setfacl -d -m g:godev:rwx /go && setfacl -d -m u::rwx /go && setfacl -d -m o::rx /go

RUN go install golang.org/x/tools/cmd/goimports@v0.43.0

# hadolint ignore=SC2016
RUN printf '%s\n' 'export PATH="/go/bin:/usr/local/go/bin:${PATH}"' \
    > /etc/profile.d/go-toolchain-path.sh && chmod 0644 /etc/profile.d/go-toolchain-path.sh

ENV DEBIAN_FRONTEND=dialog
CMD ["/bin/bash"]

# --------------------------------------------------------------------- dev ---
FROM ci AS dev

USER root
ENV DEBIAN_FRONTEND=noninteractive
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ENV DLV_VERSION=v1.27.0 \
    GOPLS_VERSION=v0.22.0 \
    STATICCHECK_VERSION=v0.7.0

RUN apt-get update \
    && apt-get -y install --no-install-recommends bash-completion \
    && apt-get autoremove -y && apt-get clean -y && rm -rf /var/lib/apt/lists/*

# hadolint ignore=SC2016
RUN printf '%s\n' \
    '' \
    'if ! shopt -oq posix; then' \
    '  if [ -f /usr/share/bash-completion/bash_completion ]; then' \
    '    . /usr/share/bash-completion/bash_completion' \
    '  fi' \
    '  if command -v task >/dev/null 2>&1 && ! complete -p task >/dev/null 2>&1; then' \
    '    eval "$(task --completion bash)"' \
    '  fi' \
    'fi' \
    >> /etc/bash.bashrc

RUN go install github.com/go-delve/delve/cmd/dlv@${DLV_VERSION} \
    && go install golang.org/x/tools/gopls@${GOPLS_VERSION} \
    && go install honnef.co/go/tools/cmd/staticcheck@${STATICCHECK_VERSION}

RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && echo 'vscode ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode \
    && usermod -aG godev vscode \
    && groupadd -f docker && usermod -aG docker vscode

RUN chown -R vscode:vscode /workspaces && chmod -R 755 /workspaces

USER vscode
ENV DEBIAN_FRONTEND=dialog
CMD ["/bin/bash"]
