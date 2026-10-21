# syntax=docker/dockerfile:1.6
#
# images/browser/Dockerfile — the first-party browser toolbox image: chromium
# plus apbrowsergate (internal/cmd/apbrowsergate), the gate that fronts it.
#
# A later task's pod builder runs TWO containers from this ONE image — Chrome
# and the gate — setting each container's Command explicitly. So this image
# needs no ENTRYPOINT contract beyond both binaries existing on PATH as a
# non-root user: /usr/bin/chromium (the debian package binary) and
# /usr/local/bin/apbrowsergate.
#
# The builder stage mirrors Dockerfile.services' own preamble (same
# golang:1.26 tag the CI pin-gate covers, same BUILDPLATFORM/TARGETARCH
# cross-compile shape, same cache mounts) even though this image builds only
# one binary — so a golang-tag bump stays a single edit across every
# first-party image.

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder
ARG TARGETARCH
WORKDIR /src
ENV GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -o /out/apbrowsergate ./internal/cmd/apbrowsergate

# --- runtime -------------------------------------------------------------------
# debian:trixie-slim, not distroless: chromium's debian package (multi-arch,
# unlike google-chrome) pulls a real dependency graph (libnss3, fontconfig,
# etc.) that a distroless/static base cannot host. fonts-liberation and
# fonts-noto-color-emoji keep common web content (Latin substitutes, emoji)
# from rendering as tofu boxes.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium fonts-liberation fonts-noto-color-emoji ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 65532 chrome
COPY --from=builder /out/apbrowsergate /usr/local/bin/apbrowsergate
# Numeric uid, not the name: Kubernetes' RunAsNonRoot:true cannot verify a
# symbolic USER from the image config and fails the container with
# CreateContainerConfigError. The useradd above still creates the named
# account so uid 65532 has a real home directory and passwd entry (Chrome
# wants both); only the recorded USER must be numeric.
USER 65532
