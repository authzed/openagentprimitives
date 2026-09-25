// derivation_test.go proves the shipped SidecarToolbox declaration
// (pkg/tools/websearch/deploy/sidecartoolbox.yaml) declares
// args.allowedFields that match this daemon's OWN live InputSchema — walking
// a REAL running server, not a transcribed list.
//
// This is the plan-8c lesson (CLAUDE.md's §Where code lives /
// SidecarToolbox conventions, and this task's own brief): the workshop
// toolbox shipped 21 tools with no allowedFields at all, so every argument
// was denied fail-closed on a real cluster
// (pkg/tools/mcp/validator/phases.go's checkAllowedFields). A hand-
// maintained list drifts the moment either side changes without the other;
// deriving the comparison from the ACTUAL wire schema this binary serves is
// what keeps it honest.
//
// This test lives in package main (internal/cmd/websearchd), not alongside
// the manifest in pkg/tools/websearch/deploy, because only this package can
// construct the real Server type that Register()s the live schema — deploy
// is a plain importable package and cannot import a package main.
// pkg/tools/websearch/deploy/sidecartoolbox_test.go carries the STATIC half
// of the same guard (every tool has a non-empty allowedFields); this file
// carries the DERIVED half.
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// declarationPath returns the path to the ONE shipped copy of this
// declaration. Not under examples/ (CLAUDE.md's test conventions forbid
// that) and not transcribed into this package — read from its single home.
func declarationPath(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Join(wd, "..", "..", "..", "pkg", "tools", "websearch", "deploy", "sidecartoolbox.yaml")
}

func loadDeclaredSpec(t *testing.T) spiceboxv1alpha1.SidecarToolboxSpec {
	t.Helper()
	data, err := os.ReadFile(declarationPath(t))
	require.NoError(t, err)
	var sp spiceboxv1alpha1.SidecarToolboxSpec
	require.NoError(t, yaml.Unmarshal(data, &sp))
	return sp
}

// liveInputSchemaProperties boots a real Server (search + fetch registered
// exactly as production's Register does), serves it over a real
// httptest.Server, connects a real MCP client, calls tools/list, and returns
// each tool's declared InputSchema property-name set. This is "walking the
// running server": the schema compared against is the one this binary
// actually advertises to an MCP client, not searchInputSchema/
// fetchInputSchema read as Go source.
func liveInputSchemaProperties(t *testing.T) map[string]map[string]bool {
	t.Helper()
	s := NewServer(&fakeExecutor{}, nil)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "derivation-test", Version: "test"}, nil)
	s.Register(mcpSrv)

	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil))
	httpSrv := httptest.NewServer(mux)
	t.Cleanup(httpSrv.Close)

	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpSrv.URL + "/mcp", HTTPClient: httpSrv.Client()}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mcpSess.Close() })

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err)

	out := make(map[string]map[string]bool, len(res.Tools))
	for _, tl := range res.Tools {
		// InputSchema comes back over the wire as `any`; the MCP client
		// json.Unmarshals the raw tools/list response into it, so a JSON
		// object always lands as map[string]any here — never the
		// *jsonschema.Schema Go value Register() constructed it from. That IS
		// the point: this reads what actually crossed the wire, not what the
		// server-side Go value happened to be.
		schema, ok := tl.InputSchema.(map[string]any)
		require.True(t, ok, "tool %q: InputSchema did not come back as a JSON object (got %T)", tl.Name, tl.InputSchema)
		props, _ := schema["properties"].(map[string]any)
		fields := make(map[string]bool, len(props))
		for k := range props {
			fields[k] = true
		}
		out[tl.Name] = fields
	}
	return out
}

// TestDeclaredAllowedFields_MatchLiveInputSchema_BothDirections is the
// derivation test the brief requires: every field the LIVE server accepts
// must be declared, and every field DECLARED must be one the live server
// actually accepts — checked as two explicit directions rather than one
// set-equality call, so a failure names which side drifted.
func TestDeclaredAllowedFields_MatchLiveInputSchema_BothDirections(t *testing.T) {
	sp := loadDeclaredSpec(t)
	live := liveInputSchemaProperties(t)

	require.ElementsMatch(t, []string{"search", "fetch"}, toolNamesOf(sp),
		"the declaration and this binary must agree on which tools exist before comparing their fields")

	for _, tool := range sp.Tools {
		liveFields, ok := live[tool.Name]
		require.True(t, ok, "declared tool %q has no live counterpart", tool.Name)

		declared := make(map[string]bool, len(tool.Args.AllowedFields))
		for _, f := range tool.Args.AllowedFields {
			declared[f] = true
		}

		for f := range liveFields {
			assert.True(t, declared[f], "tool %q: live server accepts field %q that the declaration does NOT allow — "+
				"the model could never pass it (checkAllowedFields denies it fail-closed) even though the wire schema invites it", tool.Name, f)
		}
		for f := range declared {
			assert.True(t, liveFields[f], "tool %q: declaration allows field %q that the LIVE server's InputSchema does not "+
				"advertise at all — a stale allowlist entry naming a field that no longer exists on the wire", tool.Name, f)
		}
	}
}

func toolNamesOf(sp spiceboxv1alpha1.SidecarToolboxSpec) []string {
	names := make([]string, 0, len(sp.Tools))
	for _, tool := range sp.Tools {
		names = append(names, tool.Name)
	}
	return names
}
