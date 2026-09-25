// tools_declaration_test.go hardens the sidecar's declared/live tool-name
// contract: nothing else in this package (or in deploy/sidecartoolbox_test.go,
// which only pins the YAML against a hand-maintained literal) cross-checks
// the MCP server's own ANNOUNCED tool set against deploy/sidecartoolbox.yaml's
// DECLARED one — so a rename that touches only the Go consts (tools_apply.go/
// tools_crud.go) or only the YAML would pass every other test in this package
// while silently drifting from what Task 3's SidecarToolbox CR actually
// declares. This file lives in package workshopmcp rather than package
// deploy_test alongside sidecartoolbox_test.go, so the live-server half of
// the cross-check can build a real Server directly (via this package's own
// newTestWorkshopServer/newTestMux helpers in server_test.go) instead of
// importing it from elsewhere; it reads the same YAML file by relative path.
package workshopmcp

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// declaredToolNames parses deploy/sidecartoolbox.yaml — the flat
// SidecarToolboxSpec this sidecar ships as its own toolbox declaration,
// relative to this package's directory the way deploy/sidecartoolbox_test.go's
// own declarationPath resolves it relative to that package's directory
// instead — and returns every spec.tools[].name.
func declaredToolNames(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("deploy", "sidecartoolbox.yaml"))
	require.NoError(t, err, "reading deploy/sidecartoolbox.yaml")
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp), "unmarshal SidecarToolboxSpec")
	names := make([]string, 0, len(sp.Tools))
	for _, tool := range sp.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// TestDeclaration_MatchesLiveServerToolSet builds the REAL server, lists its
// tools over the wire (exactly as TestServer_HealthAndToolsList does), and
// requires that set to match deploy/sidecartoolbox.yaml's declared set
// exactly — so either half of a rename left undone (consts changed, YAML
// not; or YAML changed, consts not) fails here by name instead of drifting
// silently into Task 3's SidecarToolbox CR.
func TestDeclaration_MatchesLiveServerToolSet(t *testing.T) {
	s := newTestWorkshopServer(t)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: "0"}, nil)
	s.Register(mcpSrv)

	srv := newTestMux(t, mcpSrv)
	served := listToolNames(t, srv)
	declared := declaredToolNames(t)

	assert.ElementsMatch(t, served, declared,
		"every served tool is declared and vice versa — no silent drift on a rename")
}

// declaredToolArgs parses deploy/sidecartoolbox.yaml and returns each tool's
// Args block, keyed by name — the allowedFields half of the same
// declaration declaredToolNames reads.
func declaredToolArgs(t *testing.T) map[string]spiceboxv1alpha1.MCPServerToolArgs {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("deploy", "sidecartoolbox.yaml"))
	require.NoError(t, err, "reading deploy/sidecartoolbox.yaml")
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp), "unmarshal SidecarToolboxSpec")
	out := make(map[string]spiceboxv1alpha1.MCPServerToolArgs, len(sp.Tools))
	for _, tool := range sp.Tools {
		out[tool.Name] = tool.Args
	}
	return out
}

// liveToolInputFieldNames connects to srv exactly as listToolNames does, and
// returns each served tool's top-level InputSchema property names, sorted —
// nil for a tool whose schema declares no properties at all (an arg-less
// tool, e.g. inventory).
func liveToolInputFieldNames(t *testing.T, srv *httptest.Server) map[string][]string {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: srv.Client()}, nil)
	require.NoError(t, err, "connect to the workshop MCP server")
	defer mcpSess.Close()

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err, "tools/list")
	out := make(map[string][]string, len(res.Tools))
	for _, tl := range res.Tools {
		out[tl.Name] = schemaPropertyNames(tl.InputSchema)
	}
	return out
}

// schemaPropertyNames extracts a JSON-Schema object's top-level property
// names, sorted. From the client, mcp.Tool.InputSchema holds the server's
// default JSON marshaling of its schema (a map[string]any) — see that
// field's own doc — so this reads it the same untyped way rather than
// assuming a generated *jsonschema.Schema.
func schemaPropertyNames(schema any) []string {
	m, ok := schema.(map[string]any)
	if !ok {
		return nil
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return nil
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// TestDeclaration_AllowedFieldsMatchLiveSchema is the drift guard task-3
// asks for: it derives the expected argument allowlist from the LIVE
// server's own InputSchema (the Go source of truth every tools_*.go file
// already declares via mcp.AddTool) and asserts
// deploy/sidecartoolbox.yaml's args.allowedFields matches EXACTLY, for
// every served tool. A hand-maintained allowlist can drift the moment a
// handler's own *Args struct gains or loses a field; this is what makes
// that impossible to do silently — the same shape as
// TestDeclaration_MatchesLiveServerToolSet above, but for arguments rather
// than names.
//
// A tool whose live schema declares no properties (inventory, export_draft,
// agents_in_thread) is required to carry NO args block at all: an allowlist
// with nothing in it is indistinguishable from "no args needed" at the
// validator (an arg-less call has nothing to reject either way — see
// pkg/tools/mcp/validator/phases.go's checkAllowedFields), so this asserts
// the stronger, more legible fact — the YAML says nothing about arguments
// for a tool that takes none.
//
// Every tool here uses args.allowedFields, never unconstrainedArgs: none of
// this sidecar's 21 tools takes genuinely free-form top-level argument
// keys — even apply's `manifest` is one fixed field name whose VALUE is an
// arbitrary object, which allowedFields already permits without needing the
// unconstrainedArgs escape hatch (see this task's report for the ruling).
func TestDeclaration_AllowedFieldsMatchLiveSchema(t *testing.T) {
	s := newTestWorkshopServer(t)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: "0"}, nil)
	s.Register(mcpSrv)
	srv := newTestMux(t, mcpSrv)

	live := liveToolInputFieldNames(t, srv)
	declared := declaredToolArgs(t)

	for name, wantFields := range live {
		args, ok := declared[name]
		require.True(t, ok, "tool %q is served but not declared in deploy/sidecartoolbox.yaml", name)
		require.False(t, args.UnconstrainedArgs,
			"tool %q opts out of the allowlist via unconstrainedArgs; none of this sidecar's tools need that escape hatch", name)

		got := append([]string(nil), args.AllowedFields...)
		sort.Strings(got)
		if len(wantFields) == 0 {
			assert.Empty(t, got, "tool %q takes no arguments; deploy/sidecartoolbox.yaml must declare no args.allowedFields for it", name)
			continue
		}
		assert.Equal(t, wantFields, got,
			"tool %q's declared args.allowedFields must match its live InputSchema property names exactly — derive, never transcribe", name)
	}
}
