# ap-toolchain-go — a relocatable-free Go toolchain overlay.
#
# The payload is built at /opt/ap-toolchains/go and mounted at
# /opt/ap-toolchains/go. Build path == mount path, so nothing inside the
# payload (shebangs, RPATHs, GOROOT autodetection) needs rewriting. Do NOT
# change either path independently.
#
# The builder stage MUST stay bookworm-based: payload binaries are dynamically
# linked and resolve libc from the *sandbox* rootfs (debian 12, see
# Dockerfile.sandbox), not from this image. A newer glibc here produces a
# `GLIBC_2.xx not found` at exec time, far from its cause.
#
# The Go version must be >= the highest `go` directive of any repo an agent is
# expected to build. Both this repo and the first consumer declare `go 1.26.6`,
# and GOTOOLCHAIN=local (below, and on the SpiceboxToolchain CR) forbids the
# `go` command from downloading a newer toolchain — so an overlay older than the
# module's `go` directive fails the build outright:
#   go: go.mod requires go >= 1.26.6 (running go 1.26.1; GOTOOLCHAIN=local)
# That is the intended, legible failure. Keep this ahead of the repos, not behind.
#
# Pinned digest. Refresh with:
#   docker pull golang:1.26.6-bookworm
#   docker image inspect golang:1.26.6-bookworm --format '{{index .RepoDigests 0}}'
FROM golang@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build

ENV TC=/opt/ap-toolchains/go
ENV GOTOOLCHAIN=local

RUN mkdir -p "${TC}" && cp -a /usr/local/go/. "${TC}/"

# gopls publishes NO prebuilt binary — golang/tools releases carry empty asset
# lists and `go install` is the only supported path. That is free here: the
# compiler is already in this stage. CGO off so the result is static and carries
# no libc dependency of its own.
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN GOROOT="${TC}" GOBIN="${TC}/tools/bin" "${TC}/bin/go" install golang.org/x/tools/gopls@v0.20.0

# Self-verify, so a broken toolchain fails the build rather than a session.
RUN GOROOT="${TC}" "${TC}/bin/go" version && "${TC}/tools/bin/gopls" version

# Prove the shipped toolchain can build a module declaring the `go` directive our
# repos declare. A version check on the string would drift; compiling the real
# thing cannot. Under GOTOOLCHAIN=local an older Go fails here at BUILD time
# instead of failing an agent's `go build` at session time.
RUN mkdir -p /tmp/goprobe && cd /tmp/goprobe \
 && printf 'module goprobe\n\ngo 1.26.6\n' > go.mod \
 && printf 'package main\n\nfunc main() {}\n' > main.go \
 && GOROOT="${TC}" GOTOOLCHAIN=local "${TC}/bin/go" build ./... \
 && cd / && rm -rf /tmp/goprobe

# No symlink in the payload may be absolute or dangling. An absolute link escapes
# the overlay and resolves against the SANDBOX rootfs — silently binding to a
# different binary, or to nothing. A dangling link means `provides:` on the CR is
# a false claim. The Go payload has none today; the guard exists because neither
# failure is visible until an agent runs the tool. (The node overlay shipped
# exactly this bug: node's `bin/nodejs -> /usr/local/bin/node` and its yarn links
# pointed outside the copied prefix.)
RUN cd "${TC}" && bad=""; \
    for l in $(find . -type l); do \
      t="$(readlink "$l")"; \
      case "$t" in /*) bad="${bad}  ${l} -> ${t} (absolute)\n";; esac; \
      [ -e "$l" ] || bad="${bad}  ${l} -> ${t} (dangling)\n"; \
    done; \
    if [ -n "$bad" ]; then printf 'payload has escaping/dangling symlinks:\n%b' "$bad"; exit 1; fi; \
    echo "symlink audit: all $(find . -type l | wc -l | tr -d ' ') links are relative and resolve"

# World-readable/executable: the init container copies as uid 1000 and cannot
# chown, and native image volumes (the future delivery kind) ignore fsGroup and
# keep in-image ownership.
RUN chmod -R a+rX "${TC}"

# busybox, not scratch: the `image` delivery kind's init container needs
# /bin/sh, mkdir, and cp. musl-static, ~1.4MB, and never executed inside the
# sandbox — only as the init container.
#
# Pinned digest. Refresh with:
#   docker pull busybox:1.37-musl
#   docker image inspect busybox:1.37-musl --format '{{index .RepoDigests 0}}'
FROM busybox@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94
COPY --from=build --chown=1000:1000 /opt/ap-toolchains/go /opt/ap-toolchains/go
