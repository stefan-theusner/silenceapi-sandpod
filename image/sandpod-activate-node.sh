#!/bin/bash
# Runs inside the sandbox before every `sandpod run`/`shell`.
#
# On the first-ever run for a given sandbox home (the persistent
# sandpod-agent-home volume), bootstraps fnm, a default LTS Node version,
# and the agent CLIs into $HOME/.fnm - cached there for every project/
# session sharing that volume from then on.
#
# Then activates the Node version pinned by /workspace's .nvmrc or
# .node-version (installing it via fnm on first use in that project). If no
# version file is present, this is a no-op and the default stays active.
#
# If /workspace has a package-lock.json but no node_modules yet, runs
# `npm ci` before handing off. node_modules is on the ignore list
# (config/global.sandignore), so the shadow filesystem routes it to the
# container-local shadow store - it's genuinely absent (ENOENT) until this
# populates it, never the host's real node_modules (which may have
# macOS-built native addons that wouldn't run in this Linux sandbox).
#
# Also ensures a colored prompt/ls (Debian's default .bashrc only colors
# the prompt when $TERM matches xterm-color/*-256color, which podman exec
# doesn't set, so the default sandbox home shows a flat, uncolored prompt).
#
# Finally execs the real command with the resulting PATH in place.
set -e

export FNM_DIR="$HOME/.fnm"
export PATH="$FNM_DIR:$FNM_DIR/aliases/default/bin:$PATH"

if [ ! -x "$FNM_DIR/fnm" ]; then
  echo "sandpod: first run for this sandbox home - installing Node toolchain (fnm + LTS + agent CLIs, ~30s)..." >&2
  curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir "$FNM_DIR" --skip-shell >/dev/null 2>&1
  fnm install --lts >/dev/null 2>&1
  fnm default "$(fnm list | grep -o 'v[0-9][0-9.]*' | tail -1)"
  eval "$(fnm env --shell bash)"
  # Verify the exact package name for Codex CLI (and any other agent CLIs)
  # from vendor docs before adding it here.
  npm install -g @anthropic-ai/claude-code >/dev/null 2>&1
  echo "sandpod: Node toolchain ready." >&2
fi

eval "$(fnm env --shell bash --use-on-cd --version-file-strategy=recursive)"
cd /workspace 2>/dev/null || true
fnm use --install-if-missing >/dev/null 2>&1 || true

if [ -f package-lock.json ] && [ ! -d node_modules ]; then
  echo "sandpod: node_modules missing - running npm ci..." >&2
  npm ci || echo "sandpod: npm ci failed, continuing anyway" >&2
fi

if ! grep -q 'sandpod_git_branch' "$HOME/.bashrc" 2>/dev/null; then
  # Drop any older, plain (no git branch) colored-prompt block a previous
  # version of this script appended, so .bashrc doesn't accumulate stale
  # duplicate PS1 assignments.
  sed -i '/# sandpod: colored prompt/,+1d' "$HOME/.bashrc" 2>/dev/null || true
  cat >> "$HOME/.bashrc" <<'EOF'

# sandpod: colored prompt (current directory in blue, git branch in yellow
# when in a repo), regardless of $TERM
sandpod_git_branch() {
  local branch
  branch="$(git branch --show-current 2>/dev/null)"
  [ -n "$branch" ] && printf ' (%s)' "$branch"
}
PS1='\[\033[01;32m\]\u@\h\[\033[00m\]:\[\033[01;34m\]\w\[\033[01;33m\]$(sandpod_git_branch)\[\033[00m\]\$ '
EOF
fi

exec "$@"
