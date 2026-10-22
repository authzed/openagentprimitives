# syntax=docker/dockerfile:1.6
# Independent example program: no OAP source or binary is included.
FROM --platform=$BUILDPLATFORM golang@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/codecovbot-tool ./cmd/codecovbot-tool

# Satisfies the sandbox contract (sh, tar, coreutils, uid 1000) and supplies
# the two read-only repository tools used by the example's git sandbox.
FROM debian@sha256:f9c6a2fd2ddbc23e336b6257a5245e31f996953ef06cd13a59fa0a1df2d5c252
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates coreutils tar git gh \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --uid 1000 --create-home bot
COPY --from=build /out/codecovbot-tool /usr/local/bin/codecovbot-tool
# Keep repository-controlled configuration from turning inspection into execution.
ENV GIT_CONFIG_COUNT=6 \
    GIT_CONFIG_KEY_0=protocol.ext.allow GIT_CONFIG_VALUE_0=never \
    GIT_CONFIG_KEY_1=protocol.ssh.allow GIT_CONFIG_VALUE_1=never \
    GIT_CONFIG_KEY_2=core.hooksPath GIT_CONFIG_VALUE_2=/dev/null \
    GIT_CONFIG_KEY_3=core.fsmonitor GIT_CONFIG_VALUE_3=false \
    GIT_CONFIG_KEY_4=core.pager GIT_CONFIG_VALUE_4=cat \
    GIT_CONFIG_KEY_5=credential.helper GIT_CONFIG_VALUE_5= \
    GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 \
    GIT_ASKPASS=/usr/local/bin/git-askpass
RUN printf '#!/bin/sh\necho "$GIT_TOKEN"\n' > /usr/local/bin/git-askpass \
    && chmod 0755 /usr/local/bin/git-askpass
USER 1000:1000
WORKDIR /tmp
