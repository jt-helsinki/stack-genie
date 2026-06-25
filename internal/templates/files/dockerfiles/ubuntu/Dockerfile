# ubuntu base — seeded into <project>/.ai-platform/Dockerfile (arch §25). Installs
# the base tooling only (Git, GitHub CLI; §12) — the same base surface as every
# other OS template. Selected software stacks and agent CLIs are appended below
# this base by `ai project create`.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git gnupg sudo tmux \
      iptables iproute2 \
 && rm -rf /var/lib/apt/lists/*

# In-VM OCI container runtime (arch §7): containerd + nerdctl + runc + CNI +
# buildkit, installed from the pinned nerdctl-full release tarball (one static,
# distro-agnostic artifact) into /usr/local. Arch-aware (amd64/arm64). The
# runtime is rootful and is started at workspace start, not in the image.
ENV NERDCTL_VERSION=2.3.3
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

# Non-root workspace user with passwordless sudo, so runtime installs work and
# persist via the overlay (arch §25, §26). (Ubuntu 24.04 ships a default `ubuntu`
# UID 1000 user; we add our own named user to keep the surface identical.)
RUN useradd --create-home --shell /bin/bash workspace \
 && echo 'workspace ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/workspace
USER workspace
WORKDIR /home/workspace
