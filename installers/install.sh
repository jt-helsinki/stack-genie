#!/usr/bin/env bash
# Thin launcher/installer for the AI Development Platform CLI (CLI spec §1.5).
#
# Its job is to place the compiled `ai` binary on disk, put its install dir on
# PATH via the shell rc file (adding or updating a single managed line), and
# source that rc so `ai` is usable immediately. No platform logic lives here —
# all real behavior is in the Go binary; run `ai setup` afterwards.
#
# Uninstall lives in the binary: run `ai uninstall` (it prompts for
# confirmation). This script installs only.
# Env: AIP_INSTALL_DIR, AIP_RELEASE_BASE_URL, AIP_VERSION, AIP_NO_MODIFY_PATH=1.
set -euo pipefail

INSTALL_DIR="${AIP_INSTALL_DIR:-$HOME/.ai-platform/bin}"
# Base URL for release artifacts named ai-<os>-<arch>. Override for forks/mirrors.
RELEASE_BASE_URL="${AIP_RELEASE_BASE_URL:-https://github.com/jt-helsinki/stack-genie/releases/latest/download}"
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

  # Supported hosts: macOS and Linux. Any other OS (e.g. msys/mingw/cygwin
  # shells) is rejected.
  case "$os" in
    darwin | linux) ;;
    *) err "unsupported OS: ${os}. This platform supports macOS (Apple Silicon) and Linux." ;;
  esac

  # macOS is supported on Apple Silicon only (arch §6.2).
  if [ "$os" = "darwin" ] && [ "$arch" != "arm64" ]; then
    err "macOS support requires Apple Silicon (Microsandbox needs the Apple Hypervisor)."
  fi

  mkdir -p "$INSTALL_DIR"

  if from_source; then
    return 0
  fi

  asset="ai-${os}-${arch}"
  info "Downloading ${asset} (${VERSION}) -> ${INSTALL_DIR}/ai"
  # Fresh inode (see from_source): avoids the macOS in-place-replace SIGKILL.
  rm -f "${INSTALL_DIR}/ai"
  download "${RELEASE_BASE_URL}/${asset}" "${INSTALL_DIR}/ai"
  verify_checksum "$asset" "${INSTALL_DIR}/ai"
  chmod +x "${INSTALL_DIR}/ai"
  done_msg
}

# verify_checksum downloads the release SHA256SUMS and checks the asset against
# it. The release publishes SHA256SUMS alongside the binaries (see
# .github/workflows/release.yml). Best-effort: if no sha256 tool or the sums
# file is unavailable, we warn and continue rather than block the install. Set
# AIP_NO_VERIFY=1 to skip entirely.
verify_checksum() {
  local asset="$1" file="$2" sums tool expected actual
  [ "${AIP_NO_VERIFY:-0}" = "1" ] && return 0

  if command -v sha256sum >/dev/null 2>&1; then
    tool="sha256sum"
  elif command -v shasum >/dev/null 2>&1; then
    tool="shasum -a 256"
  else
    info "No sha256 tool found; skipping checksum verification."
    return 0
  fi

  sums="$(mktemp)" || { info "Could not stage checksums; skipping verification."; return 0; }
  if ! download "${RELEASE_BASE_URL}/SHA256SUMS" "$sums" 2>/dev/null; then
    info "Could not fetch SHA256SUMS; skipping checksum verification."
    rm -f "$sums"
    return 0
  fi

  # SHA256SUMS lines are "<hash>  <asset>". Pull the expected hash for our asset.
  expected="$(awk -v a="$asset" '$2 == a { print $1 }' "$sums")"
  rm -f "$sums"
  if [ -z "$expected" ]; then
    info "No checksum entry for ${asset}; skipping verification."
    return 0
  fi

  actual="$($tool "$file" | awk '{ print $1 }')"
  if [ "$actual" != "$expected" ]; then
    rm -f "$file"
    err "checksum mismatch for ${asset} (expected ${expected}, got ${actual})"
  fi
  info "Checksum verified (${asset})."
}

# from_source builds the binary when run inside the source tree with Go present
# (dev convenience). Returns 0 if it handled installation. When the script is
# piped (curl | bash) there is no source file, so it returns 1 and the caller
# falls through to downloading a release asset.
from_source() {
  local src here repo
  src="${BASH_SOURCE[0]:-}"
  [ -n "$src" ] || return 1
  here="$(cd "$(dirname "$src")" 2>/dev/null && pwd)" || return 1
  repo="$(cd "$here/.." 2>/dev/null && pwd)" || return 1
  if [ -f "$repo/go.mod" ] && command -v go >/dev/null 2>&1; then
    info "Source tree detected; building from $repo"
    ( cd "$repo" && make build VERSION="$VERSION" )
    # Replace via a fresh inode: overwriting a signed arm64 binary in place can
    # trip the macOS code-signing cache and get the new binary SIGKILLed ("killed: 9").
    rm -f "${INSTALL_DIR}/ai"
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
  ensure_on_path
  info ""
  info "Recommended: install the Ghostty terminal emulator — https://ghostty.org"
  info "  (macOS: brew install --cask ghostty). Workspace microVMs are headless, so"
  info "  the in-VM agent CLIs are rendered by your terminal; Ghostty displays them"
  info "  correctly. WezTerm or a recent iTerm2 also work; Terminal.app does not."
  info ""
  info "Next:  ai setup"
}

# ensure_on_path adds (or updates in place) the PATH export for INSTALL_DIR in the
# user's shell rc file. It is idempotent: a previously-installed line — tagged
# with PATH_MARKER — is replaced rather than duplicated, so re-running the
# installer (or changing AIP_INSTALL_DIR) keeps a single, correct entry. Set
# AIP_NO_MODIFY_PATH=1 to skip and only print the manual instruction.
PATH_MARKER="# added by ai installer (AI Development Platform)"
ensure_on_path() {
  if [ "${AIP_NO_MODIFY_PATH:-0}" = "1" ]; then
    print_manual_path
    return 0
  fi

  local shell_name rc line sourceable
  shell_name="$(basename "${SHELL:-}")"
  sourceable=0  # whether `.`-sourcing the rc from this shell is valid (POSIX rc)
  case "$shell_name" in
    zsh)
      rc="${ZDOTDIR:-$HOME}/.zshrc"
      line="export PATH=\"$INSTALL_DIR:\$PATH\"  $PATH_MARKER"
      sourceable=1 ;;
    bash)
      # macOS login shells read ~/.bash_profile; Linux interactive reads ~/.bashrc.
      if [ "$(uname -s)" = "Darwin" ]; then rc="$HOME/.bash_profile"; else rc="$HOME/.bashrc"; fi
      line="export PATH=\"$INSTALL_DIR:\$PATH\"  $PATH_MARKER"
      sourceable=1 ;;
    fish)
      # fish syntax is not `.`-sourceable from this POSIX installer.
      rc="${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish"
      line="set -gx PATH \"$INSTALL_DIR\" \$PATH  $PATH_MARKER" ;;
    *)
      info "Unrecognized shell '${shell_name:-unknown}'; not modifying any rc file."
      print_manual_path
      return 0 ;;
  esac

  update_rc "$rc" "$line"
  if [ "$sourceable" = "1" ]; then
    reload_rc "$rc" "$shell_name"
  else
    info "Restart your shell (or run 'exec $shell_name') to pick up ai."
  fi
}

# update_rc writes the managed PATH line to rc, replacing any existing managed
# line (matched by PATH_MARKER) so the file never accumulates duplicates.
update_rc() {
  local rc="$1" line="$2" tmp
  mkdir -p "$(dirname "$rc")"
  touch "$rc"
  if grep -qF "$PATH_MARKER" "$rc"; then
    tmp="$(mktemp "${rc}.XXXXXX")"
    grep -vF "$PATH_MARKER" "$rc" >"$tmp"
    printf '%s\n' "$line" >>"$tmp"
    mv "$tmp" "$rc"
    info "Updated ai PATH entry in $rc"
  else
    printf '\n%s\n' "$line" >>"$rc"
    info "Added ai to PATH in $rc"
  fi
}

# reload_rc sources the rc into the current shell so the new PATH applies without
# a manual step. This persists into the user's shell only when the installer is
# itself sourced (`. install.sh`); when executed normally (or via `make install`,
# curl | sh, etc.) it only primes THIS subprocess, so the user's own interactive
# shell still needs to pick up the new PATH — hence we always recommend
# `exec <shell>` (which replaces the current shell with a fresh login that reads
# the rc). An rc can legitimately exit non-zero or reference unset vars
# (interactive guards), so errexit/nounset are relaxed around it.
reload_rc() {
  local rc="$1"
  local shell_name="${2:-}"
  local activate="exec ${shell_name:-\$SHELL}"
  set +eu
  # shellcheck disable=SC1090
  . "$rc" >/dev/null 2>&1
  set -eu
  if command -v ai >/dev/null 2>&1; then
    info "Sourced $rc — ai is on PATH here. In your current shell, run '$activate' to activate it."
  else
    info "Sourced $rc. If 'ai' isn't found, run '$activate' (or restart your shell)."
  fi
}

print_manual_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) info "Add to PATH:  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

info() { printf '%s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# Allow sourcing for tests without running the installer (AIP_SOURCE_ONLY=1).
# Uninstall is owned by the binary (`ai uninstall`, which prompts for
# confirmation) — this script installs only.
if [ "${AIP_SOURCE_ONLY:-0}" != "1" ]; then
  main "$@"
fi
