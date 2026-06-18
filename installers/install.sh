#!/usr/bin/env bash
# Thin launcher/installer for the AI Development Platform CLI (CLI spec §1.5).
#
# Its ONLY job is to place the compiled `ai` binary on disk and exec-able — no
# platform logic lives here. All real behavior is in the Go binary; run
# `ai setup` afterwards to install/configure the platform.
set -euo pipefail

INSTALL_DIR="${AIP_INSTALL_DIR:-$HOME/.ai-platform/bin}"
# Base URL for release artifacts named ai-<os>-<arch>. Override for forks/mirrors.
RELEASE_BASE_URL="${AIP_RELEASE_BASE_URL:-https://github.com/jt-helsinki/ideal-robot/releases/latest/download}"
VERSION="${AIP_VERSION:-latest}"

main() {
  local os arch asset
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    arm64 | aarch64) arch="arm64" ;;
    x86_64 | amd64)  arch="amd64" ;;
    *) err "unsupported architecture: $arch" ;;
  esac

  # Slice 1 supports macOS on Apple Silicon only (arch §6.2). Other targets
  # land in later slices; warn rather than hard-fail so dev builds work.
  if [ "$os" = "darwin" ] && [ "$arch" != "arm64" ]; then
    err "macOS support requires Apple Silicon (Microsandbox needs the Apple Hypervisor)."
  fi

  mkdir -p "$INSTALL_DIR"

  if from_source; then
    return 0
  fi

  asset="ai-${os}-${arch}"
  info "Downloading ${asset} (${VERSION}) -> ${INSTALL_DIR}/ai"
  download "${RELEASE_BASE_URL}/${asset}" "${INSTALL_DIR}/ai"
  chmod +x "${INSTALL_DIR}/ai"
  done_msg
}

# from_source builds the binary when run inside the source tree with Go present
# (dev convenience). Returns 0 if it handled installation.
from_source() {
  local here repo
  here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  repo="$(cd "$here/.." && pwd)"
  if [ -f "$repo/go.mod" ] && command -v go >/dev/null 2>&1; then
    info "Source tree detected; building from $repo"
    ( cd "$repo" && make build VERSION="$VERSION" )
    cp "$repo/bin/ai" "${INSTALL_DIR}/ai"
    chmod +x "${INSTALL_DIR}/ai"
    done_msg
    return 0
  fi
  return 1
}

download() {
  local url="$1" dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO "$dest" "$url"
  else
    err "need curl or wget to download $url"
  fi
}

done_msg() {
  info "Installed: ${INSTALL_DIR}/ai"
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) info "Add to PATH:  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
  info "Next:  ai setup"
}

info() { printf '%s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; exit 1; }

main "$@"
