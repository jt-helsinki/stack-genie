#!/usr/bin/env bash
# Thin launcher/installer for the AI Development Platform CLI (CLI spec §1.5).
#
# Its job is to place the compiled `ai` binary on disk, put its install dir on
# PATH via the shell rc file (adding or updating a single managed line), and
# source that rc so `ai` is usable immediately. No platform logic lives here —
# all real behavior is in the Go binary; run `ai setup` afterwards.
#
# Uninstall:  ./install.sh --uninstall [--purge]
#   reverses the install (binary, PATH/completion rc lines, aip-* containers);
#   --purge also removes ~/.ai-platform and ~/.clawpatrol. Never touches ~/projects.
# Env: AIP_INSTALL_DIR, AIP_RELEASE_BASE_URL, AIP_VERSION, AIP_NO_MODIFY_PATH=1.
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
  info "Next:  ai setup"
}

# ensure_on_path adds (or updates in place) the PATH export for INSTALL_DIR in the
# user's shell rc file. It is idempotent: a previously-installed line — tagged
# with PATH_MARKER — is replaced rather than duplicated, so re-running the
# installer (or changing AIP_INSTALL_DIR) keeps a single, correct entry. Set
# AIP_NO_MODIFY_PATH=1 to skip and only print the manual instruction.
PATH_MARKER="# added by ai installer (AI Development Platform)"
COMPLETION_MARKER="# added by ai completion (AI Development Platform)"
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
    reload_rc "$rc"
  else
    info "Restart your shell (or re-source $rc) to pick up ai."
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
# itself sourced (`. install.sh`); when executed normally it primes this process
# and we still hint to restart. An rc can legitimately exit non-zero or reference
# unset vars (interactive guards), so errexit/nounset are relaxed around it.
reload_rc() {
  local rc="$1"
  set +eu
  # shellcheck disable=SC1090
  . "$rc" >/dev/null 2>&1
  set -eu
  if command -v ai >/dev/null 2>&1; then
    info "Sourced $rc — ai is on PATH."
  else
    info "Sourced $rc. If 'ai' isn't found, restart your shell or run:  source \"$rc\""
  fi
}

print_manual_path() {
  case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *) info "Add to PATH:  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
  esac
}

# uninstall reverses the installer: stop platform containers, remove the binary,
# the completion scripts, and the managed rc lines. It NEVER touches ~/projects
# (your source). With --purge it also removes platform state (~/.ai-platform and
# ~/.clawpatrol). It does NOT remove msb/clawpatrol — those were installed by
# their own installers and may be used elsewhere.
uninstall() {
  local purge=0 arg
  for arg in "$@"; do
    [ "$arg" = "--purge" ] && purge=1
  done

  if command -v docker >/dev/null 2>&1; then
    local containers
    containers="$(docker ps -aq --filter 'name=aip-' 2>/dev/null || true)"
    if [ -n "$containers" ]; then
      # shellcheck disable=SC2086
      docker rm -f $containers >/dev/null 2>&1 || true
      info "Removed platform containers (aip-*)"
    fi
  fi

  if [ -f "${INSTALL_DIR}/ai" ]; then
    rm -f "${INSTALL_DIR}/ai"
    info "Removed ${INSTALL_DIR}/ai"
  fi

  rm -f "$HOME/.zsh/completions/_ai" \
        "${XDG_CONFIG_HOME:-$HOME/.config}/fish/completions/ai.fish" \
        "${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions/ai" \
        "${XDG_CONFIG_HOME:-$HOME/.config}/powershell/ai.completion.ps1" 2>/dev/null || true

  remove_rc_markers

  if [ "$purge" = "1" ]; then
    rm -rf "$HOME/.ai-platform" "$HOME/.clawpatrol"
    info "Purged ~/.ai-platform and ~/.clawpatrol (project sources under ~/projects were left untouched)"
  else
    info "Left ~/.ai-platform, ~/.clawpatrol, and ~/projects in place — re-run with --purge to remove platform state (never your projects)."
  fi
  info "Note: msb (Microsandbox) and clawpatrol were installed separately; remove them with their own tools if desired."
  info "Uninstalled. Restart your shell to drop the stale PATH entry."
}

# remove_rc_markers strips the managed PATH line and the completion block (marker
# + its fpath/compinit or dot-source lines) from the known shell rc files.
remove_rc_markers() {
  local rc tmp
  for rc in "${ZDOTDIR:-$HOME}/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" \
            "${XDG_CONFIG_HOME:-$HOME/.config}/fish/config.fish"; do
    [ -f "$rc" ] || continue
    grep -qF "$PATH_MARKER" "$rc" || grep -qF "$COMPLETION_MARKER" "$rc" || continue
    tmp="$(mktemp "${rc}.XXXXXX")"
    awk -v pm="$PATH_MARKER" -v cm="$COMPLETION_MARKER" '
      index($0, pm) { next }                       # drop the PATH line
      index($0, cm) { incomp = 1; next }           # enter the completion block
      incomp && ($0 ~ /^fpath=/ || $0 ~ /compinit/ || $0 ~ /^\. / || $0 ~ /^[[:space:]]*$/) {
        if ($0 ~ /^[[:space:]]*$/) incomp = 0       # a blank line ends the block
        next
      }
      incomp { incomp = 0 }                         # any other line ends the block (and is kept)
      { print }
    ' "$rc" >"$tmp"
    mv "$tmp" "$rc"
    info "Removed ai entries from $rc"
  done
}

info() { printf '%s\n' "$*" >&2; }
err()  { printf 'error: %s\n' "$*" >&2; exit 1; }

# Allow sourcing for tests without running the installer (AIP_SOURCE_ONLY=1).
if [ "${AIP_SOURCE_ONLY:-0}" != "1" ]; then
  case "${1:-}" in
    uninstall | --uninstall) uninstall "$@" ;;
    *) main "$@" ;;
  esac
fi
