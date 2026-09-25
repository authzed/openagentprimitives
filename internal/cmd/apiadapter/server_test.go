package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/apiadapter"
)

// newTestMux wires the same two handlers main.go's mux does — "/healthz" and
// the MCP Streamable HTTP handler at path — around mcpSrv, so a test can
// exercise them exactly as a real request would.
func newTestMux(t *testing.T, mcpSrv *mcp.Server, path string) *httptest.Server {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// listToolNames connects a real go-sdk MCP client to srv (an in-process mux
// built by newTestMux) and returns every tool name a tools/list call
// announces. This is call-routing coverage stays with Task 1's engine tests
// (pkg/tools/apiadapter): the engine's HTTP-client seam isn't reachable from
// this package, and its real guarded client refuses loopback destinations
// anyway, so a call-routing test here would need to fake the seam this
// package cannot see. What this DOES prove, honestly: the surface a real MCP
// client observes over the wire is exactly what Register wired up — no more,
// no less.
func listToolNames(t *testing.T, srv *httptest.Server, path string) []string {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + path, HTTPClient: srv.Client()}, nil)
	require.NoError(t, err, "connect to the api-adapter MCP server")
	defer mcpSess.Close()

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err, "tools/list")
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestRegister_AdvertisesExactlyTheConfiguredOperations is the boot proof:
// the server stands up, /healthz reports 200, and a real go-sdk MCP client's
// tools/list against it announces exactly the operations the config
// declares — no hand-written tool, nothing extra, nothing missing.
func TestRegister_AdvertisesExactlyTheConfiguredOperations(t *testing.T) {
	cfg, err := apiadapter.Parse([]byte(`
baseURL: https://api.example.test
auth: {type: none}
operations:
  - {name: get_account, description: Fetch one., method: GET, path: "/a/{id}", params: [{name: id, in: path, required: true, type: string}]}
  - {name: list_accounts, description: List them., method: GET, path: /a}
`))
	require.NoError(t, err)

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	NewServer(cfg, "").Register(mcpSrv)

	srv := newTestMux(t, mcpSrv, "/mcp")
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err, "GET /healthz")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "/healthz must report 200")

	// Enumerate the registered tools through the server's own listing so the
	// assertion is about what a CLIENT sees, not our internal bookkeeping.
	got := listToolNames(t, srv, "/mcp")
	assert.ElementsMatch(t, []string{"get_account", "list_accounts"}, got)
}

// TestRegister_InputSchemaIsDerivedFromParams pins that the schema a client
// sees for one operation is the operation's own params, not something
// hand-written — a get_account tool with a required "id" path param must
// advertise exactly that.
func TestRegister_InputSchemaIsDerivedFromParams(t *testing.T) {
	cfg, err := apiadapter.Parse([]byte(`
baseURL: https://api.example.test
auth: {type: none}
operations:
  - {name: get_account, description: Fetch one., method: GET, path: "/a/{id}", params: [{name: id, in: path, required: true, type: string, description: "Account ID"}]}
`))
	require.NoError(t, err)

	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	NewServer(cfg, "").Register(mcpSrv)

	srv := newTestMux(t, mcpSrv, "/mcp")
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: srv.Client()}, nil)
	require.NoError(t, err)
	defer mcpSess.Close()

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err)
	require.Len(t, res.Tools, 1)
	tl := res.Tools[0]
	assert.Equal(t, "get_account", tl.Name)
	assert.Equal(t, "Fetch one.", tl.Description)

	schema, ok := tl.InputSchema.(map[string]any)
	require.True(t, ok, "InputSchema must round-trip as a JSON object")
	assert.Equal(t, "object", schema["type"])
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok)
	idProp, ok := props["id"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", idProp["type"])
	assert.Equal(t, "Account ID", idProp["description"])
	required, ok := schema["required"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"id"}, required)
}

// TestToolErr pins the shared error-result shape every tool-call failure in
// this binary uses: IsError true, a single text block, the formatted
// message — never a bare Go error to the transport.
func TestToolErr(t *testing.T) {
	res := toolErr("get_account: %s", "boom")
	require.True(t, res.IsError)
	require.Len(t, res.Content, 1)
	text, ok := res.Content[0].(*mcp.TextContent)
	require.True(t, ok, "toolErr's content block must be text")
	assert.Equal(t, "get_account: boom", text.Text)
}

// TestDecodeArgs covers the three shapes a tool call's raw arguments can
// take: absent entirely (nil request/Params), empty, and a real JSON object
// — the same nil-safety decodeArgs's workshop counterpart documents.
func TestDecodeArgs(t *testing.T) {
	var args map[string]any

	require.NoError(t, decodeArgs(nil, &args))
	assert.Nil(t, args)

	require.NoError(t, decodeArgs(&mcp.CallToolRequest{}, &args))
	assert.Nil(t, args)

	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: []byte(`{"id":"acc-1"}`)}}
	require.NoError(t, decodeArgs(req, &args))
	assert.Equal(t, map[string]any{"id": "acc-1"}, args)
}
