# sandpod

Run AI coding agents (Claude Code, Codex, etc.) inside a hardened, rootless
Podman sandbox on macOS.

## How it works

The sandbox bind-mounts your real project directory read-write, directly.
There's only one copy of your files — edits made inside the sandbox appear
on your Mac instantly, and edits you make on your Mac are visible inside
the sandbox instantly. No sync step, no separate volume, no staging.

On top of that, specific paths are **shadowed**: hidden from the agent
entirely, based on `config/global.sandignore` (plus an optional per-project
`.sandignore`). This is enforced *live*, by a small FUSE filesystem
(`sandpod-fuse` in `fuse/`, vendored from `claude-container`/`rp-fuse` — a
sibling project that uses the same mechanism for Apple's `container` tool)
running inside the container, mounted at `/workspace`. It checks the
ignore rules on every single file access, not once at container-creation
time — so a `.env` you create ten minutes into
an already-open `sandpod shell`/`claude` session is masked exactly as if it
had existed from the start. Reading a shadowed path returns "not found"
until the *container* writes something there itself (e.g. `npm ci`
populating a shadowed `node_modules`) — that write goes to a container-local
shadow store, never to your real files.

The container's `/workspace` view is composed from two places under the
hood: your real project directory (bind-mounted into a permission-locked
spot only the FUSE process can reach, `/var/lib/sandpod/real/workspace-real`)
for everything not on the ignore list, and `/var/lib/sandpod/shadow`
(container-local, never touches the host) for everything that is. See
"Privilege model" below for how that boundary is actually enforced.

**The tradeoff, by design:** non-shadowed files are still live, direct
edits with no built-in undo layer for them — `git status`/`diff`/`checkout`
(or backups, for anything not in git) is your safety net there, not the
sandbox. This tool's job is keeping secrets out of the agent's view and
hardening what it can do to the rest of the system — not staging or
reviewing ordinary file edits.

This design went through several iterations — a copy-on-write overlay
mount, then a volume-copy-plus-manual-sync workflow, then a live
bind-mount with `/dev/null`/empty-dir masking that turned out unable to
protect a file created while a session was already open. See
`/Users/theusi/.claude/plans/i-want-to-use-jolly-plum.md` for the full
history if you're curious why each of those didn't stick.

### Privilege model

The container starts as **root**, with `CAP_SYS_ADMIN` and `/dev/fuse` —
needed to set up the tmpfs/FUSE boundary (mounting a filesystem requires
that capability). This is a real, deliberate change from a fully
`--cap-drop=ALL` posture. But the agent session itself
(`sandpod run`/`shell`) always `exec`s as the unprivileged `sandbox` user
afterward, with zero capabilities of its own — confirmed by testing:
`sandbox` gets `Permission denied` reaching `/var/lib/sandpod/real` or
`/var/lib/sandpod/shadow` directly, and `CapEff` is all-zero for that
session. Root's job is exclusively the init-time FUSE setup (plus some
bookkeeping like syncing `~/.claude` — see below); it never runs your code
or the agent.

## Prerequisites

```sh
brew install podman
podman machine init --provider applehv
podman machine start
```

(`applehv` uses macOS's built-in Virtualization framework. Podman 6's
default provider, `libkrun`, needs a `krunkit` binary from a third-party
Homebrew tap — skip it unless you've decided to trust that tap yourself.)

The default machine virtiofs-shares your home directory, so projects under
`$HOME` work out of the box. To sandbox something outside `$HOME`, you must
recreate the machine with `podman machine init --provider applehv --volume
<path>` (Podman can't add mounts to an existing machine).

This `applehv` machine runs an SELinux-enforcing VM. Named volumes (like
`sandpod-agent-home`) need the `:z` mount flag (already set in
`bin/sandpod`) or newly-written executables inside them fail with
`Permission denied` even though the Unix permission bits look correct —
SELinux denies the exec, not the filesystem. You shouldn't need to do
anything about this yourself; it's noted here in case you extend `sandpod`
and hit the same surprise.

Add this repo's `bin/` to your `PATH` so you can run `sandpod` from
anywhere:

```sh
export PATH="/Users/theusi/Development/ext/sand-pod/bin:$PATH"
```

## Build the image

```sh
sandpod build
```

Rebuild whenever you change the `Containerfile` (e.g. to add another agent
CLI — check the vendor's current npm package name before adding it). If you
already have a running sandbox container for a project, it won't pick up a
rebuilt image until you `sandpod remove` and start it again.

## Usage

Run these from inside the project directory you want to sandbox.

```sh
sandpod init             # create a .sandignore for this project (optional)
sandpod shell             # interactive shell in the sandbox
sandpod run claude        # run Claude Code directly
sandpod run codex         # run Codex CLI directly (once installed in the image)
sandpod remove            # stop and remove this project's sandbox container
sandpod list              # see every project that currently has a sandbox
sandpod config ls         # show the merged ignore list and what it masks
sandpod help              # list all commands
```

Resource limits default to 4 GiB memory / 2 CPUs; override with
`SANDPOD_MEM` / `SANDPOD_CPUS` env vars.

### Container lifecycle

`sandpod run`/`sandpod shell` create one persistent container per project
(named from the project path) the first time you use them there, and reuse
it after that — anything installed or configured inside the sandbox's
`$HOME` (auth tokens, dotfiles, etc.) survives across sessions, and so does
anything the agent wrote into a shadowed path (e.g. `node_modules`) between
sessions, until you `sandpod remove`.

The one thing that *isn't* live is the ignore list itself: `sandpod-fuse`
parses `config/global.sandignore` + your project's `.sandignore` once, when
the container starts, and holds that ruleset in memory from then on — so it
correctly catches any file matching an *existing* rule no matter when it's
created (that's the live part), but editing the ignore list itself (adding
or removing a pattern) needs a restart to take effect, the same limitation
`claude-container`'s `rp` documents for its own `.rp/shadow`. `sandpod`
handles this automatically: every `run`/`shell` hashes the current merged
ignore-list content and compares it against a label on the existing
container; on a mismatch it prints
`sandpod: ignore list changed - recreating sandbox container` and rebuilds
before proceeding, so you never have to remember to `sandpod remove`
yourself after editing `.sandignore`.

### Masking additional paths

Add a `.sandignore` file to your project root — `sandpod init` creates one
with usage comments — or hand-write it (same format as
`config/global.sandignore`): one path per line, `#` for comments, trailing
`/` for directories. It's merged with the global defaults, not a
replacement for them. Check what's actually being masked right now with
`sandpod config ls`.

### Agent config/auth state

The agent CLIs' home directory (npm/CLI config, auth tokens) lives in a
separate Podman volume, `sandpod-agent-home`, shared and reused across all
projects so you don't have to re-authenticate every session. To reset it:

```sh
podman volume rm sandpod-agent-home
```

### Your Claude Code skills, plugins, and scripts

Every `sandpod run`/`shell` copies a curated, known-safe slice of your real
`~/.claude` into the sandbox's own `~/.claude` via `podman cp` — not a mount,
so it's a one-way, disposable snapshot refreshed on every invocation, never
a live link back to your real files:

- `skills/`, `plugins/`, `commands/`, `agents/` (whichever exist)
- `CLAUDE.md`, `settings.json`, `settings.local.json`
- any top-level `*.sh` scripts (e.g. `disconnect_vpn.sh`, referenced by hooks
  or your statusline command)

Everything else in `~/.claude` — session transcripts (`projects/`),
`history.jsonl`, `shell-snapshots/`, `session-env/`, credentials, caches,
etc. — is never copied or made visible to the sandbox at all. This is an
explicit allowlist, not "copy everything and mask the sensitive parts",
because `~/.claude` accumulates a lot of unrelated cross-project history
that a sandboxed agent has no reason to see.

The sandboxed Claude Code has its own independent `~/.claude` state layered
on top of this (its own `sessions/`, `history.jsonl`, credentials once you
log in inside the sandbox, etc.) — copying in your skills/plugins doesn't
touch or replace that.

**Caveat:** scripts written for your Mac may not work unmodified in the
Linux sandbox — e.g. a VPN hook script calling `scutil`/`networksetup` will
fail with `command not found` inside the container (harmless if the script
handles that gracefully, but worth knowing).

To add more items to this allowlist, edit `sync_claude_config()` in
`bin/sandpod`.

### Node version management

Node itself isn't baked into the image. `sandpod run`/`shell` always go
through `image/sandpod-activate-node.sh` inside the container first, which:

1. On the very first run ever (empty `sandpod-agent-home` volume),
   bootstraps [fnm](https://github.com/Schniz/fnm), a default LTS Node
   version, and the agent CLIs into `$HOME/.fnm` — a one-time ~30s cost,
   cached in that volume for every project/session from then on.
2. Looks for a `.nvmrc` or `.node-version` file in your project and, if
   found, installs (if needed) and activates that exact Node version for
   this session — matching what a human developer would get running
   locally with fnm/nvm.
3. If no version file is present, the default LTS version stays active.
4. If your project has a `package-lock.json` but no `node_modules` yet,
   runs `npm ci` before handing off (every invocation checks; it's a no-op
   once `node_modules` exists).

Globally-installed agent CLIs (`claude`, `codex`) stay reachable regardless
of which per-project Node version is active — they're kept on `PATH`
independently of fnm's per-project version switching.

### node_modules isolation

`node_modules/` is in the default `config/global.sandignore`, using the
same live shadow mechanism as secrets — but the intent is different: not to
hide it from the agent, but to force a clean, container-native install
rather than reusing whatever's in your host's `node_modules` (host-installed
packages can carry native addons compiled for macOS, which won't run inside
the Linux sandbox). Your real `node_modules` on the host is never read or
written by the sandbox; `sandpod-activate-node.sh` runs `npm ci` into the
shadow store whenever it's missing.

Unlike the persistent `$HOME` volume, the shadow store lives inside the
container's own filesystem, not a separate volume — it survives
`sandpod`-managed stop/start (the container keeps running between
`shell`/`run` invocations), but `sandpod remove` deletes it along with the
container, and the next `run`/`shell` starts with a fresh, empty
`node_modules` and reruns `npm ci`.

### Colored shell

`sandpod-activate-node.sh` also patches `~/.bashrc` (idempotently, once)
with a colored prompt, since Debian's default one only colors the prompt
when `$TERM` matches `xterm-color`/`*-256color`, and `podman exec` sets
`$TERM=xterm`, so it silently stayed monochrome otherwise. `ls --color=auto`
was already working — that part doesn't depend on the exact `$TERM` value.

The prompt is `user@host` in green, the working directory in blue, and —
when the current directory is inside a git repo — the current branch in
yellow, e.g. `sandbox@6b660a5b331d:/workspace (main)$`. Blank outside a git
repo. If you already have a sandbox home from before this was added, the
old plain (no branch) prompt block is automatically replaced, not
duplicated.

### File ownership

Real project files, reached through virtiofs, are natively reported as
owned by `root` no matter who owns them on the Mac side — confusing in
`ls -l`, and enough to make git's dubious-ownership check (2.35+) refuse
every git command with
`fatal: detected dubious ownership in repository at '/workspace'`, since
that's a mismatch against the unprivileged `sandbox` user actually running
your commands. `sandpod-fuse` is told the `sandbox` user's uid/gid
(`--uid`/`--gid`, resolved by `sandpod-init.sh`) and reports that as the
owner of every file under `/workspace` instead of the real backing owner —
so `ls -l` shows `sandbox sandbox` and git just works, with nothing to
configure. This only changes what's *reported*; real access to the host
tree already goes through `sandpod-fuse` itself (running as root), not a
permission check against the caller, so it doesn't change what anyone can
actually do.
