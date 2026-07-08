# AGENTS.md

Instructions for AI coding agents (Claude Code, Codex, opencode, etc.)
working in this repository. This is the source repo for `sandpod` itself —
not a project running *inside* a sandpod sandbox.

## What this project is

`sandpod` runs AI coding agents inside a hardened Podman container, with a
live FUSE-based shadow filesystem that hides configured paths (`.env`,
`.ssh/`, `node_modules/`, etc.) from the sandboxed process — even files
created *after* the container starts. See `README.md` for the full user-facing
explanation of the architecture before making changes; don't re-derive it
from scratch.

## Repo map

- `bin/sandpod` — the CLI (bash). All command dispatch, container lifecycle,
  ignore-list hashing.
- `Containerfile` — two-stage build: compiles `fuse/` for Linux, then builds
  the runtime image (Debian) with the FUSE binary, init script, and default
  ignore list baked in.
- `fuse/` — Go source for `sandpod-fuse`, the binary that enforces the shadow
  filesystem. `main.go` (flags/wiring), `host_node.go` (the actual
  mask/passthrough logic + owner override), `rules.go` (gitignore-subset
  pattern matcher). Has real unit tests (`*_test.go`).
- `init/sandpod-init.sh` — runs as PID 1 (root) inside the container at
  startup; sets up the shadow boundary and execs `sandpod-fuse`.
- `image/sandpod-activate-node.sh` — runs before every `run`/`shell`;
  fnm bootstrap, per-project Node version, `npm ci`, colored prompt.
- `config/global.sandignore` — default mask list shipped in the image.
- `install.sh` — curl-installable installer (macOS + Linux + WSL2).

## Build & test

```sh
cd fuse
GOOS=linux go build ./...   # must target Linux — see gotcha below
GOOS=linux go vet ./...
```

**Gotcha:** `fuse/` only builds/vets cleanly with `GOOS=linux`. On a macOS
host, `syscall.Stat_t.Dev` is `int32`; on Linux it's `uint64`. Plain
`go build`/`go test` on a Mac fails with a type-mismatch compile error that
looks like a real bug but isn't — it's a host/target platform mismatch, not
a code defect. To actually *run* `fuse/*_test.go`, you need a Linux
environment (a Linux VM/container, or run natively if your host is already
Linux) — a macOS host can cross-compile for Linux but can't execute the
resulting binary.

```sh
bash -n bin/sandpod install.sh   # syntax-check shell changes before anything else
```

There is no test suite for `bin/sandpod`, `install.sh`, or the shell scripts
under `init/`/`image/` — verify changes to these by actually running them
(`podman build`, `sandpod shell`, `sandpod run <cmd>`) against a real
project, not by inspection alone.

## Working in this codebase

- **This is a security boundary, not a convenience tool.** Its entire job is
  to keep specific host files invisible to a sandboxed process. A plausible
  fix that hasn't been exercised against the actual failure scenario is not
  a fix — see the project's `.env`-visibility bug history in git log for
  what "looked right but wasn't" costs here. When touching `fuse/`,
  `init/sandpod-init.sh`, or the privilege/capability flags in
  `bin/sandpod`'s `podman run` invocation, reproduce the concrete scenario
  (e.g. create a masked file *while* a session is already open, then check
  it's still hidden) rather than trusting that the code should work.
- **Don't guess at unverified platforms.** Rootless Podman + FUSE on native
  Linux and WSL2 is explicitly flagged in `README.md` as unverified (no
  Linux/Windows test machine available). If you're asked to extend
  Linux/Windows support, add capability and surface real errors — don't
  invent troubleshooting steps or fallback behavior for failure modes no one
  has actually observed.
- **Shell portability matters.** `bin/sandpod` and `install.sh` run on both
  macOS's bash 3.2 and Linux's bash/coreutils — avoid GNU-only flags
  (e.g. `readlink -f`) and prefer constructs that work on both (see the
  manual symlink-resolution loop and `sandpod_sha256()`'s
  `sha256sum`-then-`shasum` fallback in `bin/sandpod` as the existing
  pattern to follow).
- **Comments explain *why*, not *what*.** This repo's existing comments
  document non-obvious platform divergences (e.g. why the wrapper directory
  exists instead of a tmpfs overlay — Podman's virtiofs rejects `mount -t
  tmpfs` over a virtiofs bind mount, unlike Apple's `container`). Match that
  style: only comment on hidden constraints or surprising behavior, not on
  what the code visibly does.
- **Keep `README.md` in sync.** It's the only user-facing doc. Any change to
  CLI commands, install steps, default ignore list, or the privilege model
  needs a matching README update in the same change.
- **Never commit or push unless explicitly asked**, even mid-task — leave
  changes staged/uncommitted and say so.
