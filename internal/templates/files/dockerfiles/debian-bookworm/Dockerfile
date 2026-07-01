# debian-bookworm base — seeded into <project>/.ai-platform/Dockerfile (arch §25).
# Installs the base tooling only (Git, GitHub CLI; §12) — the same base surface as
# every other OS template. Selected software stacks and agent CLIs are appended
# below this base by `ai project create`.
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    gnupg \
    less \
    nano \
    sudo \
    tmux \
    ncurses-term \
    iptables \
    iproute2 \
 && rm -rf /var/lib/apt/lists/*

# In-VM OCI container runtime (arch §7): containerd + nerdctl + runc + CNI +
# buildkit, installed from the pinned nerdctl-full release tarball (one static,
# distro-agnostic artifact) into /usr/local. Arch-aware (amd64/arm64). The
# runtime is rootful and is started at workspace start, not in the image.
ENV NERDCTL_VERSION=2.3.4
RUN arch="$(uname -m)" \
 && case "$arch" in \
      x86_64)  nerdctl_arch=amd64 ;; \
      aarch64) nerdctl_arch=arm64 ;; \
      *) echo "unsupported arch: $arch" >&2; exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-full-${NERDCTL_VERSION}-linux-${nerdctl_arch}.tar.gz" \
      -o /tmp/nerdctl-full.tar.gz \
 && tar -C /usr/local -xzf /tmp/nerdctl-full.tar.gz \
 && rm -f /tmp/nerdctl-full.tar.gz

# GitHub CLI (gh).
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# Python 3 (the base image's latest), installed system-wide; python3-venv backs the
# per-project /workspace/.venv-msb created at workspace start.
RUN apt-get update \
 && apt-get install -y --no-install-recommends python3 python3-venv python3-pip \
 && rm -rf /var/lib/apt/lists/*

# Non-root workspace user with passwordless sudo, so runtime installs work and
# persist via the overlay (arch §25, §26).
RUN useradd --create-home --shell /bin/bash workspace \
 && echo 'workspace ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/workspace
# uv + Graphify install onto the workspace user's PATH (~/.local/bin) below, so
# each agent-CLI snippet can register Graphify with its assistant.
ENV PATH="/home/workspace/.local/bin:${PATH}"
USER workspace
WORKDIR /home/workspace

# uv (Astral's Python package/tool manager) + Graphify — installed by default in
# every workspace, for the WORKSPACE USER. Graphify (github.com/safishamsi/graphify,
# PyPI `graphifyy`) is a knowledge-graph skill for AI coding assistants; the optional
# extras are bundled (all except the region/DB-specific chinese, azure, bedrock,
# falkordb, neo4j, leiden, dm). Each selected agent-CLI snippet then registers it via
# `graphify install --platform …`. Verify the extras resolve on hardware bring-up.
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
 && uv tool install "graphifyy[pdf,office,video,postgres,google,svg,sql,terraform,ollama,openai,gemini,anthropic]"

# Keep-alive command. Microsandbox boots the microVM running the IMAGE'S command,
# so the workspace image needs a long-running one — otherwise the base image's
# default shell exits immediately on the empty stdin of a detached VM and msb stops
# the sandbox right after boot. All real work runs via `msb exec` (the tmux
# sessions / agent CLIs); this just keeps the microVM up.
CMD ["sleep", "infinity"]
