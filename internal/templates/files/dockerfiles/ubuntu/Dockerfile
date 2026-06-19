# ubuntu base — seeded into <project>/.ai-platform/Dockerfile (arch §25). Installs
# the base tooling only (Git, GitHub CLI; §12) — the same base surface as every
# other OS template. Selected software stacks and agent CLIs are appended below
# this base by `ai project create`.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl git gnupg sudo \
 && rm -rf /var/lib/apt/lists/*

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
