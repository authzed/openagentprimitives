// Command apiadapter is ap-api-adapter: a declarative HTTP→MCP adapter. It
// reads an API config from AP_SIDECAR_CONFIG, serves one MCP tool per
// configured operation over Streamable-HTTP, and executes each call against
// the upstream API with one injected credential.
//
// It runs no user code: the config is data, never a script. See
// pkg/tools/apiadapter for the engine (config parsing, param binding,
// SSRF-guarded HTTP client) this binary wraps.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// buildVersion is the MCP Implementation.Version this server reports to a
// connecting client. Not currently wired to -ldflags; a fixed string is
// enough until this binary's own release story exists.
var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("ap-api-adapter: %v", err)
	}
}

// run reads the env contract, builds the Server, and serves. Every
// precondition failure here is fatal and explicit — a sidecar that cannot
// prove it has a valid config, a resolved credential, and a port to bind
// must not start serving a partial tool surface.
func run() error {
	cfg, credential, port, path, err := loadFromEnv()
	if err != nil {
		return err
	}

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-api-adapter", Version: buildVersion}, nil)
	NewServer(cfg, credential).Register(mcpSrv)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)

	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":" + port
	log.Printf("ap-api-adapter %s: listening on %s%s, serving %d operation(s)", buildVersion, addr, path, len(cfg.Operations))
	srv := &http.Server{Addr: addr, Handler: mux}
	safehttp.HardenServer(srv)
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// loadFromEnv reads the env contract run() needs to build a Server: a valid
// AP_SIDECAR_CONFIG, the credential its auth scheme names (when configured),
// MCP_PORT, and MCP_PATH (defaulted). Split out from run() so the refusal
// branches — otherwise unreachable behind ListenAndServe — are testable.
//
// The credential is read here, not in the engine: the binary owns the
// environment, the engine stays pure. It is TrimSpace'd: a Secret populated
// via `--from-file` carries the file's trailing newline verbatim, and
// "Bearer "+cred with that newline fails net/http's header validation with a
// value-omitted error on every call.
func loadFromEnv() (cfg apiadapter.Config, credential, port, path string, err error) {
	raw := os.Getenv("AP_SIDECAR_CONFIG")
	if raw == "" {
		return apiadapter.Config{}, "", "", "", fmt.Errorf("AP_SIDECAR_CONFIG is required")
	}
	cfg, err = apiadapter.Parse([]byte(raw))
	if err != nil {
		return apiadapter.Config{}, "", "", "", fmt.Errorf("parse AP_SIDECAR_CONFIG: %w", err)
	}

	if cfg.Auth.Type != "none" {
		credential = strings.TrimSpace(os.Getenv(cfg.Auth.EnvVar))
		if credential == "" {
			return apiadapter.Config{}, "", "", "", fmt.Errorf("auth.envVar %q is set in the config but empty in the environment", cfg.Auth.EnvVar)
		}
	}

	port = os.Getenv("MCP_PORT")
	if port == "" {
		return apiadapter.Config{}, "", "", "", fmt.Errorf("MCP_PORT is required")
	}
	path = mcpPathFromEnv()

	return cfg, credential, port, path, nil
}

// mcpPathFromEnv returns MCP_PATH, defaulting to "/mcp" when unset.
func mcpPathFromEnv() string {
	if p := os.Getenv("MCP_PATH"); p != "" {
		return p
	}
	return "/mcp"
}
