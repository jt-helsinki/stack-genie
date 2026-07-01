# alma base (AlmaLinux 10) — seeded into <project>/.ai-platform/Dockerfile
# (arch §25). Installs the base tooling only (Git, GitHub CLI; §12) — the same
# base surface as every other OS template, via dnf rather than apt. Selected
# software stacks and agent CLIs are appended below this base by
# `ai project create`.
FROM almalinux:10

RUN dnf install -y --setopt=install_weak_deps=False \
      ca-certificates curl git sudo tmux tar gzip nano less git ncurses-term \
      iptables-nft iproute \
 && dnf clean all

# In-VM OCI container runtime (arch §7): containerd + nerdctl + runc + CNI +
# buildkit, installed from the pinned nerdctl-full release tarball (one static,
# distro-agnostic artifact) into /usr/local. Arch-aware (amd64/arm64). The
# runtime is rootful and is started at workspace start, not in the image. (On
# RHEL-family the iptables provider is iptables-nft and the package is iproute.)
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

# GitHub CLI (gh) from the official GitHub CLI rpm repo.
RUN dnf install -y 'dnf-command(config-manager)' \
 && dnf config-manager --add-repo https://cli.github.com/packages/rpm/gh-cli.repo \
 && dnf install -y --setopt=install_weak_deps=False gh \
 && dnf clean all

# Non-root workspace user with passwordless sudo, so runtime installs work and
# persist via the overlay (arch §25, §26).
RUN useradd --create-home --shell /bin/bash workspace \
 && echo 'workspace ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/workspace
USER workspace
WORKDIR /home/workspace
