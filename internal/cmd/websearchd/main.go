// Command websearchd is ap-websearchd: a client-dispatched search-and-fetch
// sidecar. It exposes two MCP tools, search and fetch, over Streamable-HTTP,
// backed by a registry-selected github.com/authzed/openagentprimitives/pkg/tools/websearch
// provider.
//
// It exists so that an agent's web access is CLIENT-dispatched rather than a
// model provider's own inline search: a server-side search/fetch result
// never becomes a tool.Result, so it never passes through the runner's
// untrusted-content wrapper, never reaches a content-guard subject, and
// never touches the toolguard byte budget — every control this platform has
// over tool output is absent by construction. Because this binary makes the
// HTTP request itself, its result IS a tool.Result like any other MCP tool
// call, and every one of those controls applies to it automatically.
//
// It runs no user code: query and URL arguments are plain strings handed to
// a provider's own HTTP client (safehttp.Client() in production), never
// interpreted as a script — the same posture internal/cmd/apiadapter states
// for its own config-driven calls.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore"
	"github.com/authzed/openagentprimitives/pkg/platform/artifactstore/blob"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/bravesearch"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch/registry"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// buildVersion is the MCP Implementation.Version this server reports to a
// connecting client. Not currently wired to -ldflags; a fixed string is
// enough until this binary's own release story exists (mirrors
// internal/cmd/apiadapter).
var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("ap-websearchd: %v", err)
	}
}

// run reads the env contract, builds the Server, and serves. Every
// precondition failure here is fatal and explicit — a sidecar that cannot
// prove it has a working search backend, a resolved credential, and a port
// to bind must not start serving a partial tool surface (mirrors
// internal/cmd/apiadapter's run()).
func run() error {
	cfg, err := loadFromEnv()
	if err != nil {
		return err
	}

	backend, ok := registry.Get(cfg.backendName)
	if !ok {
		return fmt.Errorf("no websearch backend registered for %q (known: %v)", cfg.backendName, registry.Keys())
	}
	provider, err := backend.New(websearch.Deps{HTTPClient: safehttp.Client(), APIKey: cfg.apiKey})
	if err != nil {
		return fmt.Errorf("construct %q backend: %w", cfg.backendName, err)
	}
	// search and fetch both dispatch straight onto SearchExecutor, not the
	// ClientTools()-based in-runner-meta-tool half of websearch.Provider
	// (that half is what R1/R2 supersede — see pkg/tools/websearch's package
	// doc). A backend that cannot execute both is not usable by this daemon.
	exec, ok := provider.(websearch.SearchExecutor)
	if !ok {
		return fmt.Errorf("backend %q does not implement websearch.SearchExecutor; it cannot serve search+fetch", cfg.backendName)
	}

	// store stays a true nil interface unless ARTIFACT_STORE_URL is
	// configured — see CLAUDE.md's nil-interface rule; a *blob.Store is
	// assigned into it only once actually constructed, never declared as a
	// nil pointer first.
	var store artifactstore.Store
	if cfg.artifactStoreURL != "" {
		s, err := blob.Open(context.Background(), cfg.artifactStoreURL)
		if err != nil {
			return fmt.Errorf("open artifact store %q: %w", cfg.artifactStoreURL, err)
		}
		store = s
	}

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-websearchd", Version: buildVersion}, nil)
	NewServer(exec, store).Register(mcpSrv)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)

	mux := http.NewServeMux()
	mux.Handle(cfg.path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":" + cfg.port
	log.Printf("ap-websearchd %s: listening on %s%s, backend=%q, artifact-store-configured=%t",
		buildVersion, addr, cfg.path, cfg.backendName, store != nil)
	srv := &http.Server{Addr: addr, Handler: mux}
	safehttp.HardenServer(srv)
	if err := srv.ListenAndServe(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

// envConfig is the env contract this binary reads.
type envConfig struct {
	backendName      string
	apiKey           string
	port             string
	path             string
	artifactStoreURL string
}

const (
	// envBackend selects which registry.Get name to construct. Optional:
	// unset defaults to bravesearch.KindName, the one production backend
	// registered today, so a plain deployment needs no opinion about this.
	envBackend = "WEBSEARCH_BACKEND"
	// envAPIKey is the credential the selected backend needs to search.
	// Required, and read ONLY here: the runner never sees this value, and no
	// tool argument may name, carry, or select it — a SidecarToolbox wiring
	// this daemon's credential must set its upstreamAuth envVar to exactly
	// this name.
	envAPIKey = "WEBSEARCH_API_KEY"
	// envArtifactStoreURL is OPTIONAL, unlike the operator's own
	// ARTIFACT_STORE_URL contract (cmd/oap/internal/installcmd's
	// validateArtifactStoreURL, which is fail-closed on empty): this sidecar
	// serves fetch's excerpt-plus-truncated behavior with no artifact store
	// at all, so its absence is a valid, if degraded, configuration rather
	// than a startup failure. A NON-empty but malformed value is still
	// refused — see blob.Open's own scheme check.
	envArtifactStoreURL = "ARTIFACT_STORE_URL"
)

// loadFromEnv reads envConfig from the process environment. Split out from
// run() so the refusal branches — otherwise unreachable behind
// ListenAndServe — are testable, exactly as internal/cmd/apiadapter's
// loadFromEnv is.
func loadFromEnv() (envConfig, error) {
	var cfg envConfig

	cfg.backendName = os.Getenv(envBackend)
	if cfg.backendName == "" {
		cfg.backendName = bravesearch.KindName
	}

	cfg.apiKey = os.Getenv(envAPIKey)
	if cfg.apiKey == "" {
		return envConfig{}, fmt.Errorf("%s is required", envAPIKey)
	}

	cfg.port = os.Getenv("MCP_PORT")
	if cfg.port == "" {
		return envConfig{}, fmt.Errorf("MCP_PORT is required")
	}

	cfg.path = os.Getenv("MCP_PATH")
	if cfg.path == "" {
		cfg.path = "/mcp"
	}

	cfg.artifactStoreURL = os.Getenv(envArtifactStoreURL)

	return cfg, nil
}
