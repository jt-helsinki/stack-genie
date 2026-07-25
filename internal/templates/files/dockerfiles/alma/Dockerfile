# alma base (AlmaLinux 10) — seeded into <project>/.ai-platform/Dockerfile
# (arch §25). Installs the base tooling only (Git, GitHub CLI; §12) — the same
# base surface as every other OS template, via dnf rather than apt. Selected
# software stacks and agent CLIs are appended below this base by
# `ai project create`.
FROM almalinux:10.2-minimal


# Base tooling + Python 3 in ONE layer. Python is a distro package (no external
# repo), so it folds in here; it backs the per-project ~/project/.venv-msb created at
# workspace start. curl/ca-certificates/gnupg are needed by the external-repo setup
# (gh, NodeSource) below, so they land in this first layer.
RUN dnf install -y --setopt=install_weak_deps=False \
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
    procps-ng \
    fuse3 \
    python3 \
    python3-pip \
 && dnf clean all

# In-VM OCI container runtime (arch §7): containerd + nerdctl + runc + CNI +
# buildkit, installed from the pinned nerdctl-full release tarball (one static,
# distro-agnostic artifact) into /usr/local. Arch-aware (amd64/arm64). The
# runtime is rootful and is started at workspace start, not in the image. (On
# RHEL-family the iptables provider is iptables-nft and the package is iproute.)
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

# GitHub CLI (gh) + Node.js 24 LTS (NodeSource rpm) in ONE layer — both just add an
# external dnf repo, so their repo setup + a single install share a layer. Node 24:
# the distro module Node is too old for the agent CLIs — pi's bundled undici calls
# webidl.markAsUncloneable, added in Node 22.10 and never backported to 20.x. Node is
# installed ONCE here so every Node-based agent CLI shares this one pinned Node (they
# no longer install it; there is no `node` stack).
ENV NODE_MAJOR=24
RUN dnf install -y 'dnf-command(config-manager)' \
 && dnf config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo \
 && curl -fsSL https://rpm.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
 && dnf install -y --setopt=install_weak_deps=False gh nodejs \
 && dnf clean all

# Non-root workspace user with passwordless sudo, so runtime installs work and
# persist via the overlay (arch §25, §26).
RUN useradd --create-home --shell /bin/bash workspace \
 && echo 'workspace ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/workspace
# uv installs onto the workspace user's PATH (~/.local/bin) below; Graphify is
# appended as a conditional tools/ snippet when selected at `ai create`.
ENV PATH="/home/workspace/.local/bin:${PATH}"
USER workspace
WORKDIR /home/workspace

# uv (Astral's Python package/tool manager) — installed by default in every workspace,
# for the WORKSPACE USER. Graphify (PyPI `graphifyy`) is NO LONGER baked here: it is an
# opt-in tools/ snippet (context.graphify_enabled, default on) appended to the project
# Dockerfile only when selected at `ai create`, then registered per agent CLI at start.
# rtk (github.com/rtk-ai/rtk, "Rust Token Killer") is also installed for the workspace
# user via its official install.sh — which auto-detects the arch (amd64/arm64) and drops the prebuilt Linux binary in
# ~/.local/bin (already on PATH; no root). It is a CLI proxy that compresses common
# dev-command output to cut agent token use. Claude Code's rtk PreToolUse hook (added
# by `rtk init`) shells out to `rtk`, so the binary must be on PATH — without it the
# hook fails with `rtk: command not found` (exit 127). NB: rtk's crates.io name
# collides with an unrelated package, so we use install.sh, never `cargo install rtk`.
# The opt-in tools graphify, code-review-graph + codebase-memory-mcp are NOT baked here.
# They are appended to the project Dockerfile as CONDITIONAL snippets (see
# internal/templates/files/tools/) only when selected at `ai create`, so an unselected
# tool's installer never runs at build. They are registered with the installed agent CLIs
# at workspace start (workspace.registerCodeReviewGraph / registerCodebaseMemory).
RUN curl -LsSf https://astral.sh/uv/install.sh | sh \
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
