#!/usr/bin/env sh
# Local installer for the AI Development Platform CLI.
#
# Installs the `ai` binary BUILT FROM THIS REPO (not a GitHub release), so you
# can test the `curl … | sh` install flow against your working tree.
#
# Usage — pick one:
#
#   # straight from the file (curl supports file://):
#   curl -fsSL file:///Users/jordan/Documents/workspace/stack-genie/installers/install-local.sh | sh
#
#   # or serve the repo over HTTP and curl it (closer to the real remote flow):
#   ( cd /Users/jordan/Documents/workspace/stack-genie && python3 -m http.server 8000 ) &
#   curl -fsSL http://localhost:8000/installers/install-local.sh | sh
#
# How it works: a script piped to `sh` has no source path, so it can't find the
# repo on its own. This baked-in launcher execs the repo's real installer
# (installers/install.sh) BY ABSOLUTE PATH, which triggers that script's
# build-from-source path: `make build`, install to ~/.ai-platform/bin, and PATH
# update. All the actual install logic lives in install.sh (single source of
# truth); this only bridges the pipe → local-repo gap.
#
# Requires Go + make on PATH. Override the repo with AIP_REPO=/path. Honors the
# same env as install.sh (AIP_INSTALL_DIR, AIP_NO_MODIFY_PATH, AIP_VERSION).
# After it finishes, run `ai setup`.
set -eu

AIP_REPO="${AIP_REPO:-/Users/jordan/Documents/workspace/stack-genie}"

if [ ! -f "$AIP_REPO/installers/install.sh" ]; then
  printf 'error: repo not found at %s — set AIP_REPO=/path/to/stack-genie\n' "$AIP_REPO" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  printf 'error: Go is required to build from source. Install Go, or use the release installer (installers/install.sh piped from curl).\n' >&2
  exit 1
fi
if ! command -v make >/dev/null 2>&1; then
  printf 'error: make is required to build from source.\n' >&2
  exit 1
fi

# Exec the real installer by path → install.sh's from_source() sees a valid
# BASH_SOURCE, builds from $AIP_REPO, installs, and updates PATH.
exec bash "$AIP_REPO/installers/install.sh" "$@"
