# alma base (AlmaLinux 10) — seeded into <project>/.ai-platform/Dockerfile
# (arch §25). Installs the base tooling only (Git, GitHub CLI; §12) — the same
# base surface as every other OS template, via dnf rather than apt. Selected
# software stacks and agent CLIs are appended below this base by
# `ai project create`.
FROM almalinux:10

RUN dnf install -y --setopt=install_weak_deps=False \
      ca-certificates curl git sudo \
 && dnf clean all

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
