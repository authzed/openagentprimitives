// Command leadflowfake runs pkg/web/uidemo/leadflow as a standalone MCP
// Streamable HTTP server, for exercising the agent-defined-UI leads console
// against a real process rather than an in-test httptest.Server.
//
// It is a `go build ./internal/cmd/leadflowfake`-and-run binary, not a container
// image: it is demo upstream for an example MCPServer CR, not a long-lived
// platform service, so it carries none of the shared-builder / Dockerfile /
// install-surface obligations a sixth image target would add.
//
// pkg/x/safehttp's SSRF guard refuses loopback/link-local/RFC1918-private
// destinations, so an MCPServer CR's spec.server.url cannot point at a
// cluster-internal Service — run this binary somewhere the cluster can
// reach it by a public address.
package main

import (
	"flag"
	"log/slog"
	"net/http"
	"os"

	"github.com/authzed/openagentprimitives/pkg/web/uidemo/leadflow"
)

func main() {
	addr := flag.String("addr", ":8730", "listen address")
	path := flag.String("path", "/mcp", "path to mount the MCP endpoint at (must match the MCPServer CR's spec.server.url path)")
	flag.Parse()

	logger := slog.Default()
	srv := leadflow.New(leadflow.Options{Logger: logger})

	mux := http.NewServeMux()
	mux.Handle(*path, srv.Handler())

	logger.Info("leadflowfake: listening", "addr", *addr, "path", *path)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		logger.Error("leadflowfake: server exited", "err", err.Error())
		os.Exit(1)
	}
}
