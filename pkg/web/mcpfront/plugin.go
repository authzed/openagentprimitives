package mcpfront

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
	webuiregistry "github.com/authzed/openagentprimitives/pkg/web/webui/registry"
)

// Version is the MCP Implementation.Version this server reports to a
// connecting client. Not currently wired to -ldflags, matching every other
// in-repo MCP server's own buildVersion var (internal/cmd/workshop,
// internal/cmd/websearchd, internal/cmd/apiadapter) — a fixed string is
// enough until this surface has its own release story.
var Version = "dev"

// ui is the /mcp webui plugin. Stateless: the mcp.Server and bearer
// middleware are built fresh inside Routes().
type ui struct{}

func init() { webuiregistry.Register(ui{}) }

func (ui) Name() string { return "mcpfront" }

// Routes mounts the /mcp plugin. It is INERT — returns no routes at all —
// when deps doesn't implement Deps, or AccessTokenAuthz() is nil: an
// unconfigured SpiceDB means no MCP surface exists, fail closed by absence
// rather than by every request refusing one at a time.
func (ui) Routes(deps webui.Deps) []webui.Route {
	d, ok := deps.(Deps)
	if !ok || d.AccessTokenAuthz() == nil {
		return nil
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "oap", Version: Version}, nil)
	registerTools(srv, d)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	authed := newBearerMiddleware(d, time.Now)(handler)

	return []webui.Route{
		{
			Origin:  webui.OriginTrusted,
			Pattern: "/mcp",
			Methods: []string{http.MethodGet, http.MethodPost, http.MethodDelete},
			// AuthHandlerManaged: the bearer middleware above authenticates by
			// its own means (the AccessToken's hash, not the session cookie),
			// and the MCP protocol itself needs GET/POST/DELETE, which
			// AuthNone's GET/HEAD-only restriction would refuse.
			Auth:    webui.AuthHandlerManaged,
			Handler: authed,
		},
		{
			Origin:  webui.OriginTrusted,
			Pattern: "/.well-known/oauth-protected-resource",
			Methods: []string{http.MethodGet},
			Auth:    webui.AuthNone,
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				base := strings.TrimSuffix(d.ExternalBaseURL(), "/")
				writeJSON(w, http.StatusOK, map[string]any{
					"resource":              base + "/mcp",
					"authorization_servers": []string{base},
				})
			}),
		},
	}
}

// writeJSON is the standard tiny json-response helper, duplicated per
// package to match this repo's existing convention (pkg/web/webui/chat,
// pkg/web/webui/agentui, pkg/web/webui/interact and pkg/web/admind each keep
// their own copy rather than share one three-line helper).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
