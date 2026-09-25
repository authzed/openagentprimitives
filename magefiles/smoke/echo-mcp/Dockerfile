# Smoke-test echo-MCP sidecar image. Built + kind-loaded by `mage test:smoke`
# as the REAL sidecar container injected into an AgentSession runner Pod.
#
# Build context is the repo ROOT (so go.mod/go.sum resolve):
#   docker build -f magefiles/smoke/echo-mcp/Dockerfile -t echo-mcp-smoke:dev .
#
# Multi-stage: static CGO-free binary → distroless nonroot. nonroot satisfies
# the sidecar securityContext (runAsNonRoot=true, readOnlyRootFilesystem=true,
# drop ALL caps) the operator applies — the server only listens on a TCP port
# and writes nothing to disk.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/echo-mcp ./magefiles/smoke/echo-mcp

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/echo-mcp /echo-mcp
USER nonroot:nonroot
ENTRYPOINT ["/echo-mcp"]
