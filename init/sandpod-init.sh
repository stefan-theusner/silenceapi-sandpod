#!/bin/bash -p
# /usr/local/bin/sandpod-init.sh
#
# Runs as PID 1 (root, CAP_SYS_ADMIN) at container start. Sets up the shadow
# boundary then launches sandpod-fuse. Adapted from claude-container's
# rp-init.sh (built for Apple `container`) with one real difference found
# during the Podman port spike: Podman's virtiofs refuses a `mount -t tmpfs`
# placed over a virtiofs-backed bind mount (Apple's virtiofs tolerates it;
# confirmed reproducible on this stack, isolated to virtiofs specifically).
# So instead of hiding /workspace-real behind a tmpfs overlay + reaching it
# via a captured fd, the image bakes a permission-restricted wrapper
# directory at build time (see Containerfile: /var/lib/sandpod/real is
# 0700, its workspace-real child is 0755) and the project is bind-mounted
# directly into that child at `podman run` time. A non-root exec can't
# traverse the 0700 parent to reach it; root (this script / sandpod-fuse)
# can, via the plain path - no fd capture, no tmpfs, no /proc/self/fd
# resolution needed.
#
# Layout:
#   /var/lib/sandpod/real/workspace-real   host bind mount (root-only, via parent perms)
#   /var/lib/sandpod/shadow                container-local writable store for shadowed paths
#   /var/lib/sandpod/rules                 merged global + project ignore list
#   /workspace                             FUSE mount that the agent/shell sees
#
# The container exits if sandpod-fuse exits.
set +e

REAL=/var/lib/sandpod/real/workspace-real
MNT=/workspace
SHADOW=/var/lib/sandpod/shadow
RULES=/var/lib/sandpod/rules
GLOBAL_IGNORE=/etc/sandpod/global.sandignore
SANDBOX_USER=sandbox

if [ ! -d "$REAL" ]; then
    echo "sandpod-init: $REAL does not exist; nothing to mount" >&2
    exec sleep infinity
fi

mkdir -p "$MNT" "$SHADOW"

# Shadow-boundary invariants (mirrors claude-container's ADR-0005/ADR-0008
# invariant 3): the sandbox user must exist, have uid != 0, and not be
# listed in any sudoers file. Belt-and-braces on top of the same checks at
# image-build time.
if ! id -u "$SANDBOX_USER" >/dev/null 2>&1; then
    echo "sandpod-init: user '$SANDBOX_USER' does not exist in image; refusing to launch" >&2
    exec sleep infinity
fi
if [ "$(id -u "$SANDBOX_USER")" = "0" ]; then
    echo "sandpod-init: user '$SANDBOX_USER' has uid 0; refusing to launch (shadow boundary requires uid != 0)" >&2
    exec sleep infinity
fi
if cat /etc/sudoers /etc/sudoers.d/* 2>/dev/null | sed 's/#.*//' \
        | grep -qE "(^|[[:space:]])${SANDBOX_USER}([[:space:]]|$)"; then
    echo "sandpod-init: user '$SANDBOX_USER' has a sudoers entry; refusing to launch (shadow boundary requires no sudo)" >&2
    exec sleep infinity
fi

# If a prior init left a FUSE mount around, drop it.
if mountpoint -q "$MNT"; then
    fusermount3 -u "$MNT" 2>/dev/null || umount -l "$MNT" 2>/dev/null
fi

# Merge global (image-baked defaults) + project-local ignore list into one
# rules file - sandpod-fuse's --rules takes a single file.
: > "$RULES"
[ -f "$GLOBAL_IGNORE" ] && cat "$GLOBAL_IGNORE" >> "$RULES"
if [ -f "$REAL/.sandignore" ]; then
    cat "$REAL/.sandignore" >> "$RULES"
    echo "sandpod-init: merged global + project .sandignore" >&2
else
    echo "sandpod-init: no project .sandignore; using global defaults only" >&2
fi

CACHE_FLAG=""
if [ -n "${SANDPOD_FUSE_CACHE:-}" ]; then
    CACHE_FLAG="--cache $SANDPOD_FUSE_CACHE"
    echo "sandpod-init: fuse cache TTL = ${SANDPOD_FUSE_CACHE}s" >&2
fi

DEBUG_FLAG=""
if [ "${SANDPOD_DEBUG:-}" = "1" ]; then
    DEBUG_FLAG="--debug"
    echo "sandpod-init: FUSE debug logging enabled" >&2
fi

# Real project files, seen through virtiofs, report as owned by root no
# matter who owns them on the Mac side - confusing in `ls -l` and enough to
# trip git's dubious-ownership check. sandpod-fuse's --uid/--gid override
# what it *reports* for the host tree to match the sandbox user; it doesn't
# change real permission enforcement, which for the host tree already
# happens via the underlying syscalls running as sandpod-fuse itself
# (root), not the caller.
SANDBOX_UID="$(id -u "$SANDBOX_USER")"
SANDBOX_GID="$(id -g "$SANDBOX_USER")"

echo "sandpod-init: launching sandpod-fuse (backing=$REAL)" >&2
exec /usr/local/bin/sandpod-fuse \
    --backing "$REAL" \
    --shadow "$SHADOW" \
    --mount "$MNT" \
    --rules "$RULES" \
    --uid "$SANDBOX_UID" \
    --gid "$SANDBOX_GID" \
    $CACHE_FLAG \
    $DEBUG_FLAG
