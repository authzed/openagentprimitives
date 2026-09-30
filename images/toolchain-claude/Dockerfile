# syntax=docker/dockerfile:1.6
#
# ap-toolchain-claude — a relocation-free Claude Code CLI overlay.
#
# The payload is built at /opt/ap-toolchains/claude and mounted at
# /opt/ap-toolchains/claude. Build path == mount path, so nothing inside the
# payload needs rewriting. Do NOT change either path independently.
#
# Claude Code ships as a single self-contained native binary (no Node.js tree).
# The official installer lays it out as a launcher symlink
# (~/.local/bin/claude) into a versioned dir
# (~/.local/share/claude/versions/<v>/claude); that machinery exists only for
# the background auto-updater, which a reproducible sandbox must not run. So we
# run the installer in the builder, then copy JUST the one versioned binary to
# the fixed prefix and discard the launcher/versions scaffolding.
#
# bin/claude is NOT that binary. The real binary is installed as bin/claude.real
# and AP's own launcher — internal/cmd/claudeshim, a small static Go binary
# built below — takes the name bin/claude instead. Claude Code cannot see
# AP-staged skill bundles (/skills/<name>/) on its own: it only discovers
# user-level skills at $HOME/.claude/skills/. claudeshim bridges the two
# (pkg/tools/toolchain/claude.LinkStagedSkills) on every invocation, then
# exec's claude.real, faithfully passing through argv/env/stdio/the exit code.
# See internal/cmd/claudeshim's package doc for the full contract, and
# config/toolchains/claude.yaml's env comment for the pointer from the CR side.
#
# Build context is the REPO ROOT (pkg/platform/apimage.ToolchainClaude), not
# this directory: claudeshim imports pkg/tools/toolchain/claude, unreachable
# from a context scoped to images/toolchain-claude/ alone. Mirrors
# Dockerfile.services/Dockerfile.sandbox; .dockerignore already trims the
# context for exactly this shape of build (see its header comment).
#
# The debian builder stays bookworm-based for the same reason as toolchain-go:
# claude.real resolves libc from the *sandbox* rootfs (debian 12, see
# Dockerfile.sandbox), so a matching glibc here avoids a `GLIBC_2.xx not found`
# at exec time. claudeshim itself is CGO_ENABLED=0/static and carries no libc
# dependency of its own — only claude.real needs the matching glibc.
#
# --- claudeshim: the launcher, built in its own stage --------------------
#
# This stage must be complete before the `build` stage below can copy the
# shim binary out of it via --from=shim. Dockerfile stages cannot be resumed:
# once a later FROM opens `build`, `shim` can no longer be extended or
# referenced without becoming a circular dependency. Keep this stage first.
#
# Static (CGO_ENABLED=0), so it carries no libc dependency and this stage's
# base need not match the sandbox rootfs the way claude.real's does below.
# --platform=$BUILDPLATFORM runs the compiler natively and cross-compiles via
# GOARCH, mirroring Dockerfile.services' shared builder stage (see its header
# comment for why: avoids paying qemu emulation for the compile itself).
#
# Pinned digest, same pin as images/toolchain-go/Dockerfile's builder stage —
# this repo's go.mod declares `go 1.26.6`, and reusing an already-verified
# digest here avoids fetching a second, unverified one for a pin whose only
# real requirement is "new enough to compile the module". Refresh with:
#   docker pull golang:1.26.6-bookworm
#   docker image inspect golang:1.26.6-bookworm --format '{{index .RepoDigests 0}}'
FROM --platform=$BUILDPLATFORM golang@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS shim
ARG TARGETARCH
WORKDIR /src
ENV GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -o /out/claude ./internal/cmd/claudeshim

# Pinned digest. Refresh with:
#   docker pull debian:12-slim
#   docker image inspect debian:12-slim --format '{{index .RepoDigests 0}}'
FROM debian@sha256:f9c6a2fd2ddbc23e336b6257a5245e31f996953ef06cd13a59fa0a1df2d5c252 AS build

ENV TC=/opt/ap-toolchains/claude
# CI avoids any interactive prompt; auto-updater off so nothing mutates on disk.
ENV DISABLE_AUTOUPDATER=1 CI=1

RUN apt-get update \
 && apt-get install -y --no-install-recommends curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*

# Native install to the default $HOME/.local, then relocate the single binary
# to the fixed prefix AS claude.real (see the launcher note above — bin/claude
# is claudeshim, built in a separate stage above and copied in next).
# `readlink -f` resolves the launcher symlink to the actual versioned binary.
# Version pin is passed to install.sh so the build is reproducible. Bump
# deliberately (the payload is ~256MB; re-measure sizeBytes on the
# SpiceboxToolchain CR if a bump changes it materially).
ARG CLAUDE_VERSION=2.1.207
RUN curl -fsSL https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}" \
 && mkdir -p "${TC}/bin" \
 && cp "$(readlink -f "$HOME/.local/bin/claude")" "${TC}/bin/claude.real" \
 && chmod 0755 "${TC}/bin/claude.real"

# Land the shim at bin/claude in the SAME stage as claude.real, before the
# symlink audit / world-readable chmod below, so both binaries get that
# treatment uniformly instead of duplicating it for a second copy target.
COPY --from=shim /out/claude "${TC}/bin/claude"
RUN chmod 0755 "${TC}/bin/claude"

# Self-verify the FULL launcher chain at the FINAL path (build path == mount
# path): this exercises claudeshim's own exec-path resolution and argv/exit-code
# passthrough against the real binary, not just claude.real in isolation, so a
# shim that can't find or hand off to claude.real fails the build, not a
# session. No /skills mount exists in this build stage, so the staged-skill
# bridge takes its documented no-op path — see internal/cmd/claudeshim's
# TestRun_DegradesLoudlyOnBridgeFailure for the loud-not-silent behavior when
# that assumption doesn't hold at session time.
RUN "${TC}/bin/claude" --version

# No symlink in the payload may be absolute or dangling (an absolute link
# escapes the overlay and resolves against the SANDBOX rootfs). Neither binary
# here is a symlink (claude.real and claude are both copied files); the guard
# exists because the failure is invisible until run.
RUN cd "${TC}" && bad=""; \
    for l in $(find . -type l); do \
      t="$(readlink "$l")"; \
      case "$t" in /*) bad="${bad}  ${l} -> ${t} (absolute)\n";; esac; \
      [ -e "$l" ] || bad="${bad}  ${l} -> ${t} (dangling)\n"; \
    done; \
    if [ -n "$bad" ]; then printf 'payload has escaping/dangling symlinks:\n%b' "$bad"; exit 1; fi; \
    echo "symlink audit: all $(find . -type l | wc -l | tr -d ' ') links are relative and resolve"

# World-readable/executable: the init container copies as uid 1000 and cannot
# chown, and native image volumes (a future delivery kind) keep in-image ownership.
RUN chmod -R a+rX "${TC}"

# busybox, not scratch: the `image` delivery kind's init container needs
# /bin/sh, mkdir, and cp. Never executed inside the sandbox.
#
# Pinned digest. Refresh with:
#   docker pull busybox:1.37-musl
#   docker image inspect busybox:1.37-musl --format '{{index .RepoDigests 0}}'
FROM busybox@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94
COPY --from=build --chown=1000:1000 /opt/ap-toolchains/claude /opt/ap-toolchains/claude
