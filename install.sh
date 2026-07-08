#!/usr/bin/env bash
# sandpod installer.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/stefan-theusner/silenceapi-sandpod/main/install.sh | bash
#
# Clones (or updates) sandpod into $SANDPOD_INSTALL_DIR (default
# ~/.sandpod), installs Podman if needed, sets up a podman machine if none
# exists, builds the sandbox image, and links the `sandpod` command onto
# your PATH.
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

[ "$(uname -s)" = "Darwin" ] || die "this installer is for macOS only"

command -v git >/dev/null 2>&1 || die "git is required (run 'xcode-select --install' first)"

if ! command -v podman >/dev/null 2>&1; then
  command -v brew >/dev/null 2>&1 || die "podman isn't installed and Homebrew isn't available - install podman first: https://podman.io/docs/installation"
  log "Installing podman via Homebrew"
  brew install podman
fi

if [ -d "$INSTALL_DIR/.git" ]; then
  log "Updating existing install at $INSTALL_DIR"
  git -C "$INSTALL_DIR" pull --ff-only
else
  log "Cloning sandpod into $INSTALL_DIR"
  git clone "$REPO_URL" "$INSTALL_DIR"
fi

if ! podman machine list --format '{{.Name}}' 2>/dev/null | grep -q .; then
  log "Initializing a podman machine (applehv)"
  podman machine init --provider applehv
fi
if ! podman machine list --format '{{.Running}}' 2>/dev/null | grep -qi true; then
  log "Starting the podman machine"
  podman machine start
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
