# debian-bookworm base — seeded into <project>/.ai-platform/Dockerfile (arch §25).
# Installs the base tooling only (Git, GitHub CLI; §12) — the same base surface as
# every other OS template. Selected software stacks and agent CLIs are appended
# below this base by `ai project create`.
FROM debian:bookworm-slim

# Base tooling + Python 3 in ONE layer. Python is a distro package (no external
# repo), so it folds in here; python3-venv backs the per-project ~/project/.venv-msb
# created at workspace start. ca-certificates/curl/gnupg are needed by the
# external-repo setup (gh, NodeSource) below, so they land in this first layer.
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
    zsh \
    ncurses-term \
    iptables \
    iproute2 \
    python3 \
    python3-venv \
    python3-pip \
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

# GitHub CLI (gh) + Node.js 24 LTS (NodeSource) in ONE layer — both just add an
# external apt repo, so their keyring/repo setup + a single install share a layer
# (NodeSource's setup script runs apt-get update, which also refreshes the gh repo
# added just before it). Node 24: the distro apt Node (18/20) is too old for the agent
# CLIs — pi's bundled undici calls webidl.markAsUncloneable, added in Node 22.10 and
# never backported to 20.x. Node is installed ONCE here so every Node-based agent CLI
# shares this one pinned Node (they no longer install it; there is no `node` stack).
ENV NODE_MAJOR=24
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
 && curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
 && apt-get install -y --no-install-recommends gh nodejs \
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
# every workspace, for the WORKSPACE USER. Graphify (github.com/Graphify-Labs/graphify,
# PyPI `graphifyy`) is a knowledge-graph skill for AI coding assistants; the optional
# extras are bundled (all incl. mcp except the region/DB/niche-specific chinese, azure,
# bedrock, falkordb, neo4j, leiden, dm, pascal). Each selected agent-CLI snippet then registers it via
# `graphify install --platform …`. Verify the extras resolve on hardware bring-up.
# rtk (github.com/rtk-ai/rtk, "Rust Token Killer") is also installed for the workspace
# user via its official install.sh — a prebuilt aarch64 Linux binary dropped in
# ~/.local/bin (already on PATH; no root). It is a CLI proxy that compresses common
# dev-command output to cut agent token use. Claude Code's rtk PreToolUse hook (added
# by `rtk init`) shells out to `rtk`, so the binary must be on PATH — without it the
# hook fails with `rtk: command not found` (exit 127). NB: rtk's crates.io name
# collides with an unrelated package, so we use install.sh, never `cargo install rtk`.
# The opt-in code-graph tools code-review-graph + codebase-memory-mcp are NOT baked here.
# They are appended to the project Dockerfile as CONDITIONAL snippets (see
# internal/templates/files/tools/) only when selected at `ai create`, so an unselected
# tool's installer never runs at build. They are registered with the installed agent CLIs
# at workspace start (workspace.registerCodeReviewGraph / registerCodebaseMemory).
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
 && uv tool install --no-cache "graphifyy[pdf,office,video,postgres,google,svg,sql,terraform,ollama,openai,gemini,anthropic,mcp]" \
 && uv tool install --no-cache "headroom-ai[proxy]" \
 && curl -fsSL https://raw.githubusercontent.com/rtk-ai/rtk/master/install.sh | sh \
 && rm -rf "$HOME/.cache/uv"

# Interactive-shell frameworks for the workspace user: oh-my-bash and oh-my-zsh, both
# installed UNATTENDED (they write ~/.bashrc / ~/.zshrc + ~/.oh-my-*). zsh itself is in
# the base layer above. The default shell (bash or zsh) is chosen per workspace at
# `ai create` and applied at start; both frameworks ship so either works. RUNZSH/CHSH=no
# stop the oh-my-zsh installer launching zsh or calling chsh at build time.
RUN bash -c "$(curl -fsSL https://raw.githubusercontent.com/ohmybash/oh-my-bash/master/tools/install.sh)" --unattended \
 && RUNZSH=no CHSH=no sh -c "$(curl -fsSL https://raw.githubusercontent.com/ohmyzsh/ohmyzsh/master/tools/install.sh)" "" --unattended

# Keep-alive command. Microsandbox boots the microVM running the IMAGE'S command,
# so the workspace image needs a long-running one — otherwise the base image's
# default shell exits immediately on the empty stdin of a detached VM and msb stops
# the sandbox right after boot. All real work runs via `msb exec` (the tmux
# sessions / agent CLIs); this just keeps the microVM up.
CMD ["sleep", "infinity"]
