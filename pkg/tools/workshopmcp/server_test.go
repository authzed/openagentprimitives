package workshopmcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/controllers/testfixtures"
)

// newTestMux wires the same two handlers main.go's mux does — "/healthz" and
// the MCP Streamable HTTP handler at mcpPathFromEnv()'s default path — around
// mcpSrv, so a test can exercise them exactly as a real request would.
// mcpPathFromEnv itself stays in internal/cmd/workshop/main.go (it reads
// MCP_PATH, an env var this package's tests never set), so this helper
// inlines its default ("/mcp") rather than calling it.
func newTestMux(t *testing.T, mcpSrv *mcp.Server) *httptest.Server {
	t.Helper()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newTestWorkshopServer builds a Server against a fake controller-runtime
// client, wired with an otherwise-empty workshop identity — the shared
// fixture behind every test in this file that needs a real, fully-registered
// mcp.Server rather than one Server method in isolation.
func newTestWorkshopServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		K8s: fake.NewClientBuilder().WithScheme(testfixtures.NewScheme(t)).Build(),
		Identity: WorkshopIdentity{
			Namespace:        "ws-abc123",
			SessionNamespace: "b",
			SessionName:      "x",
			WorkshopID:       "ws-abc123",
		},
		FieldOwner: defaultFieldOwner,
	}
}

// listToolNames connects a real go-sdk MCP client to srv (an in-process mux
// built by newTestMux) and returns every tool name a tools/list call
// announces. Shared by TestServer_HealthAndToolsList (which only cares that
// inventory is among them) and TestDeclaration_MatchesLiveServerToolSet in
// tools_declaration_test.go (which cares that the set is EXACTLY the
// SidecarToolbox YAML's declared set).
func listToolNames(t *testing.T, srv *httptest.Server) []string {
	t.Helper()
	ctx := context.Background()
	c := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcpSess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: srv.Client()}, nil)
	require.NoError(t, err, "connect to the workshop MCP server")
	defer mcpSess.Close()

	res, err := mcpSess.ListTools(ctx, nil)
	require.NoError(t, err, "tools/list")
	names := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

// TestServer_HealthAndToolsList is the boot proof: the server stands up,
// /healthz reports 200, and a real go-sdk MCP client's tools/list against it
// succeeds and announces every tool Register wires on — inventory as of Task
// 2, more from later tasks.
func TestServer_HealthAndToolsList(t *testing.T) {
	s := newTestWorkshopServer(t)
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "ap-workshop", Version: "0"}, nil)
	s.Register(mcpSrv)

	srv := newTestMux(t, mcpSrv)
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err, "GET /healthz")
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode, "/healthz must report 200")

	names := listToolNames(t, srv)
	assert.Contains(t, names, toolInventory, "Register must wire the inventory tool")
}

// TestNewServer_DefaultsFieldOwner proves the empty-string shortcut every
// production call site but a test would otherwise have to spell out.
func TestNewServer_DefaultsFieldOwner(t *testing.T) {
	s := NewServer(nil, nil, WorkshopIdentity{}, "")
	assert.Equal(t, defaultFieldOwner, s.FieldOwner)

	s2 := NewServer(nil, nil, WorkshopIdentity{}, "custom-owner")
	assert.Equal(t, "custom-owner", s2.FieldOwner, "an explicit FieldOwner is preserved")
}

// TestServer_ToolErrAndDeniedResult pins the two structured-error shapes
// every workshop tool failure rides on: toolErr's {"error": "..."} and
// deniedResult's {"denied": true, "message": "..."} — surfacing an apiserver
// denial VERBATIM, never re-worded.
func TestServer_ToolErrAndDeniedResult(t *testing.T) {
	s := &Server{}

	errRes := s.toolErr("boom: %d", 42)
	require.True(t, errRes.IsError)
	require.Len(t, errRes.Content, 1)
	text, ok := errRes.Content[0].(*mcp.TextContent)
	require.True(t, ok, "toolErr's content block must be text")
	assert.JSONEq(t, `{"error":"boom: 42"}`, text.Text)

	denyRes := s.deniedResult(errors.New("apiserver: forbidden"))
	require.True(t, denyRes.IsError)
	require.Len(t, denyRes.Content, 1)
	text2, ok := denyRes.Content[0].(*mcp.TextContent)
	require.True(t, ok, "deniedResult's content block must be text")
	assert.JSONEq(t, `{"denied":true,"message":"apiserver: forbidden"}`, text2.Text)

	// A nil error still produces a well-formed denied body, never a panic.
	nilDenyRes := s.deniedResult(nil)
	text3, ok := nilDenyRes.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.JSONEq(t, `{"denied":true,"message":""}`, text3.Text)
}
