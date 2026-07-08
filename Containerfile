FROM golang:1.22-alpine AS fuse-build
WORKDIR /src
COPY fuse/go.mod fuse/go.sum ./
RUN go mod download
COPY fuse/main.go fuse/host_node.go fuse/rules.go ./
RUN CGO_ENABLED=0 go build -o /out/sandpod-fuse .

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    curl \
    ca-certificates \
    build-essential \
    python3 \
    python3-pip \
    ripgrep \
    unzip \
    fuse3 \
    less \
    vim \
    nano \
    && rm -rf /var/lib/apt/lists/*
RUN echo "user_allow_other" >> /etc/fuse.conf

RUN useradd --create-home --shell /bin/bash sandbox

# Live shadow filesystem (see README "How it works"): sandpod-fuse (vendored
# from claude-container/rp-fuse) mounts /workspace, merging real host
# content for everything not on the ignore list with a container-local
# shadow store for everything that is - enforced live, per file access, not
# just at container-creation time.
#
# /var/lib/sandpod/real is 0700 root:root, baked in here at build time,
# before any runtime mount ever touches its workspace-real child - the
# project directory is bind-mounted straight into that child at `podman
# run` time. A non-root exec can't traverse the 0700 parent to reach it;
# only root (this init process / sandpod-fuse) can. This is a Podman-
# specific adaptation: the original rp-init.sh instead hid the bind mount
# behind a tmpfs overlay + reached it via a captured fd, but Podman's
# virtiofs (unlike Apple Container's) refuses a tmpfs mount placed over a
# virtiofs-backed path - confirmed by direct testing.
RUN mkdir -p /var/lib/sandpod/real/workspace-real /var/lib/sandpod/shadow /workspace \
    && chmod 700 /var/lib/sandpod/real \
    && chmod 755 /var/lib/sandpod/real/workspace-real
COPY --from=fuse-build /out/sandpod-fuse /usr/local/bin/sandpod-fuse
COPY init/sandpod-init.sh /usr/local/bin/sandpod-init.sh
RUN chmod +x /usr/local/bin/sandpod-init.sh
COPY config/global.sandignore /etc/sandpod/global.sandignore

WORKDIR /workspace

# Node itself is NOT installed here. fnm, a default Node version, and the
# agent CLIs are bootstrapped lazily at runtime by
# sandpod-activate-node.sh, into $HOME/.fnm - because $HOME is the
# sandpod-agent-home volume (mounted over /home/sandbox at container
# start), anything installed under /home/sandbox at image-build time would
# be invisible at runtime anyway, shadowed by that volume mount. Building
# it lazily means it's built exactly where it'll actually be used, and
# cached there across every project/session sharing that volume.
COPY --chown=sandbox:sandbox image/sandpod-activate-node.sh /usr/local/bin/sandpod-activate-node.sh

# Container starts as root (PID 1 = sandpod-init.sh) - it needs CAP_SYS_ADMIN
# to mount the FUSE filesystem. The actual agent/shell session execs
# afterward as the unprivileged `sandbox` user (see bin/sandpod), which
# never runs as root and never gets CAP_SYS_ADMIN.
CMD ["/usr/local/bin/sandpod-init.sh"]
