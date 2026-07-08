#!/usr/bin/env bash
# sandpod installer.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/stefan-theusner/silenceapi-sandpod/main/install.sh | bash
#
# Clones (or updates) sandpod into $SANDPOD_INSTALL_DIR (default
# ~/.sandpod), installs Podman if needed (on macOS: Homebrew + a podman
# machine VM; on Linux: your distro's package manager, no VM needed since
# Podman runs natively there), builds the sandbox image, and links the
# `sandpod` command onto your PATH.
#
# Windows: run this from inside a WSL2 distro (e.g. Ubuntu), not from
# PowerShell/cmd - WSL2 is a real Linux kernel and reports as plain "Linux",
# so it's handled identically to bare-metal Linux below. There's no native
# (non-WSL) Windows support - this is a bash script and the whole tool
# assumes a Linux container backend.
#
# Linux note (WSL2 included): FUSE-mounting under a distro's default
# (usually rootless) Podman setup hasn't been verified across
# distros/kernels yet - if `sandpod shell`/`run` fails to mount /workspace,
# that's the current known gap, not something this installer can detect or
# work around for you.
#
# Env vars:
#   SANDPOD_INSTALL_DIR  where to clone the repo (default: $HOME/.sandpod)
#   SANDPOD_BIN_DIR       where to link the `sandpod` command (default: /usr/local/bin)
#   SANDPOD_REPO_URL      git remote to clone (default: this project's GitHub repo)
set -euo pipefail

REPO_URL="${SANDPOD_REPO_URL:-https://github.com/stefan-theusner/silenceapi-sandpod.git}"
INSTALL_DIR="${SANDPOD_INSTALL_DIR:-$HOME/.sandpod}"
BIN_DIR="${SANDPOD_BIN_DIR:-/usr/local/bin}"

log() { echo "==> $*" >&2; }
die() { echo "sandpod install: $*" >&2; exit 1; }

OS="$(uname -s)"
case "$OS" in
  Darwin|Linux) ;;
  *) die "unsupported OS '$OS' - sandpod supports macOS and Linux (Windows via WSL2, which reports as Linux)" ;;
esac

# WSL2 reports uname -s as plain "Linux", so it already takes the Linux
# branch below with no special-casing needed - Windows support IS Linux
# support, run from inside a WSL2 distro. This is purely an informational
# note for two WSL-specific gotchas that don't apply to bare-metal Linux.
if [ -n "${WSL_DISTRO_NAME:-}" ] || grep -qi microsoft /proc/version 2>/dev/null; then
  log "Detected WSL2 (${WSL_DISTRO_NAME:-unknown distro}) - two things worth knowing:"
  log "  1. For best performance/reliability, keep projects inside the WSL"
  log "     filesystem (e.g. ~/projects/...) rather than a Windows path under"
  log "     /mnt/c/... - cross-boundary access works but is slower."
  log "  2. FUSE-mounting under WSL2's kernel is unverified, same open gap"
  log "     as bare-metal Linux (see README) - possibly more so, since WSL2's"
  log "     kernel has its own quirks distinct from a real Linux distro's."
fi

command -v git >/dev/null 2>&1 || die "git is required$([ "$OS" = Darwin ] && echo " (run 'xcode-select --install' first)")"

install_podman_macos() {
  command -v brew >/dev/null 2>&1 || die "podman isn't installed and Homebrew isn't available - install podman first: https://podman.io/docs/installation"
  log "Installing podman via Homebrew"
  brew install podman
}

install_podman_linux() {
  if command -v apt-get >/dev/null 2>&1; then
    log "Installing podman via apt-get (may prompt for sudo)"
    sudo apt-get update && sudo apt-get install -y podman
  elif command -v dnf >/dev/null 2>&1; then
    log "Installing podman via dnf (may prompt for sudo)"
    sudo dnf install -y podman
  else
    die "podman isn't installed and no supported package manager (apt-get, dnf) was found - install podman yourself: https://podman.io/docs/installation"
  fi
}

if ! command -v podman >/dev/null 2>&1; then
  if [ "$OS" = Darwin ]; then install_podman_macos; else install_podman_linux; fi
fi

if [ -d "$INSTALL_DIR/.git" ]; then
  log "Updating existing install at $INSTALL_DIR"
  git -C "$INSTALL_DIR" pull --ff-only
else
  log "Cloning sandpod into $INSTALL_DIR"
  git clone "$REPO_URL" "$INSTALL_DIR"
fi

# Podman on Linux runs natively on the host kernel - no VM/machine step.
# This is macOS (and Windows) only.
if [ "$OS" = Darwin ]; then
  if ! podman machine list --format '{{.Name}}' 2>/dev/null | grep -q .; then
    log "Initializing a podman machine (applehv)"
    podman machine init --provider applehv
  fi
  if ! podman machine list --format '{{.Running}}' 2>/dev/null | grep -qi true; then
    log "Starting the podman machine"
    podman machine start
  fi
fi

log "Building the sandpod image (first build can take a few minutes)"
podman build -t sandpod "$INSTALL_DIR"

chmod +x "$INSTALL_DIR/bin/sandpod"
if [ -w "$BIN_DIR" ] 2>/dev/null; then
  ln -sf "$INSTALL_DIR/bin/sandpod" "$BIN_DIR/sandpod"
  log "Linked $BIN_DIR/sandpod -> $INSTALL_DIR/bin/sandpod"
  log "Done. Run 'sandpod help' to get started."
else
  log "Done, but $BIN_DIR isn't writable - add this to your shell profile instead:"
  echo ""
  echo "    export PATH=\"$INSTALL_DIR/bin:\$PATH\""
  echo ""
  log "Then restart your shell and run 'sandpod help' to get started."
fi
