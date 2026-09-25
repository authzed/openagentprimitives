package mcp_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/mcp"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
	mcptest "github.com/authzed/openagentprimitives/pkg/tools/mcp/testing"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMCPTool_PrependDescription(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "gh"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "list_issues"}}
	live := []probe.Tool{{Name: "list_issues", Description: "List all open issues"}}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err)
	tools := res.LLMTools
	require.Len(t, tools, 1)

	mt, ok := tools[0].(*mcp.MCPTool)
	require.True(t, ok, "Synthesize must return *mcp.MCPTool")
	assert.Equal(t, "List all open issues", mt.Description(), "description before prepend")
	mt.PrependDescription("⚠ DRIFT WARNING: ")
	assert.Equal(t, "⚠ DRIFT WARNING: List all open issues", mt.Description(),
		"Description must reflect the prepended prefix")
}

func TestMCPTool_Introspect_RendersContract(t *testing.T) {
	const (
		constraintCEL = "args.limit <= 50"
		constraintMsg = "limit must not exceed 50 issues per call"
	)

	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "linear"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{
		{
			Name:   "list_issues",
			Intent: "list issues for a team",
			Args: spiceboxv1alpha1.MCPServerToolArgs{
				AllowedFields:   []string{"teamId", "limit"},
				SensitiveFields: []string{"teamId"},
				Constraints: []spiceboxv1alpha1.MCPServerConstraint{
					{CEL: constraintCEL, Message: constraintMsg},
				},
			},
		},
	}
	live := []probe.Tool{
		{Name: "list_issues", Description: "List issues"},
	}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	tools := res.LLMTools
	require.Len(t, tools, 1, "tools")

	intro, ok := agenttool.Tool(tools[0]).(agenttool.Introspectable)
	require.True(t, ok, "MCPTool must implement tool.Introspectable")

	out, err := intro.Introspect()
	require.NoError(t, err)
	assert.NotEmpty(t, out)
	// Constraint Message is surfaced to the LLM; raw CEL never is.
	assert.Contains(t, out, constraintMsg, "constraint Message should appear")
	assert.NotContains(t, out, constraintCEL, "raw CEL must not be emitted")
}

// TestMCPTool_SetUseTokenGate verifies the gate accessor pair: a nil gate by
// default (the check is disabled until the runner wires one), SetUseTokenGate
// stores the gate, and it is returned unchanged.
func TestMCPTool_SetUseTokenGate(t *testing.T) {
	cr := &spiceboxv1alpha1.MCPServer{}
	cr.Name = "gh"
	cr.Spec.Server.URL = "https://x"
	cr.Spec.Tools = []spiceboxv1alpha1.MCPServerTool{{Name: "list_issues"}}
	live := []probe.Tool{{Name: "list_issues"}}

	res, err := mcp.Synthesize(cr, live)
	require.NoError(t, err, "Synthesize")
	mt := res.LLMTools[0].(*mcp.MCPTool)

	assert.Nil(t, mt.UseTokenGateForTest(), "gate must be nil until SetUseTokenGate is called")

	gate := &mcp.UseTokenGate{
		SessionNS:   "ns",
		SessionName: "sess",
		CredID:      "cred-id",
	}
	mt.SetUseTokenGate(gate)
	assert.Same(t, gate, mt.UseTokenGateForTest(), "currentUseTokenGate must return the exact gate SetUseTokenGate stored")
}

// TestMCPTool_Execute_SurfacesUpstreamHTTPStatus pins the MCP half of the
// credential-update corroboration carrier: a failed MCP-origin tool call must
// expose the upstream HTTP status via Result.HTTPStatus -- an out-of-band
// field, never the LLM-facing Content -- extracted with errors.As on
// *probe.HTTPError (dispatch.go), not by parsing the error string. A
// non-HTTP failure (nothing ever answered, so there is no status to
// observe) must leave HTTPStatus at its zero value; the downstream
// classifier (pkg/platform/identity/credupdate.Observation) reads zero as "nothing
// observed", never as a counterfeit real status.
func TestMCPTool_Execute_SurfacesUpstreamHTTPStatus(t *testing.T) {
	// rawServer answers every request with a fixed HTTP status/body — enough
	// to fault the go-sdk client's initialize handshake and force the status
	// through authTransport.recordIfHTTPError into a *probe.HTTPError.
	rawServer := func(status int, body string) func(t *testing.T) string {
		return func(t *testing.T) string {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(body))
			}))
			t.Cleanup(srv.Close)
			return srv.URL
		}
	}
	// unreachableServer stands up a listener and closes it immediately, so the
	// returned URL has nothing behind it: connecting fails at the transport
	// (connection refused) with NO HTTP response at all — the case where
	// there is genuinely no status to observe, as opposed to the rows above
	// where a real (if unhappy) response comes back.
	unreachableServer := func(t *testing.T) string {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		return srv.URL
	}

	cases := []struct {
		name       string
		newServer  func(t *testing.T) string
		wantStatus int
	}{
		{
			name:       "upstream 401: Result.HTTPStatus surfaces 401",
			newServer:  rawServer(http.StatusUnauthorized, `{"error":"unauthorized"}`),
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "upstream 500: Result.HTTPStatus surfaces 500",
			newServer:  rawServer(http.StatusInternalServerError, "oops"),
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "non-HTTP failure (connection refused): Result.HTTPStatus stays 0, not a counterfeit status",
			newServer:  unreachableServer,
			wantStatus: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tools := mustSynthesize(t, tc.newServer(t))
			_, opID, sess := newOpAndSess(t)
			res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
			require.NoError(t, err, "Execute must surface the failure via Result, not a Go error")
			require.True(t, res.IsError, "expected a failed call for this row")
			assert.Equal(t, tc.wantStatus, res.HTTPStatus, "Result.HTTPStatus")
		})
	}
}

// TestMCPTool_Execute_ReportsTheOriginAsAuthenticated is the positive half of
// the same carrier, and the one that keeps a stale auth-failure observation
// from outliving a credential that has since started working.
//
// A completed JSON-RPC exchange means the server answered HTTP 200 and the
// endpoint's auth layer accepted the credential. That is true even when the
// server sets the tool-level isError -- that flag is the SERVER's opinion of
// the arguments, and the agent chooses the arguments. Gating the positive
// carrier on !IsError would let a prompt-injected agent hold a dead
// observation open indefinitely by sending arguments it knows will fail; for a
// provider with no verify: probe, corroboration is what decides whether a
// human is shown a credential-entry form.
//
// The failed-call rows above are the mirror: nothing completed, so nothing may
// claim the credential authenticated.
func TestMCPTool_Execute_ReportsTheOriginAsAuthenticated(t *testing.T) {
	t.Run("HTTP 200 with a tool-level isError still reports the origin as authenticated", func(t *testing.T) {
		srv := mcptest.NewCallServer(mcptest.CallServerOpts{
			Tools: map[string]mcptest.ToolHandler{
				"search_issues": mcptest.StaticTool(mcptest.TextResult("no such repository", true)),
			},
		})
		t.Cleanup(srv.Close)

		tools := mustSynthesize(t, srv.URL)
		_, opID, sess := newOpAndSess(t)
		res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
		require.NoError(t, err, "Execute")

		require.True(t, res.IsError, "precondition: the server returned a tool-level error inside a 200")
		assert.True(t, res.OriginAuthenticated,
			"the exchange completed, so the credential got past the endpoint's auth layer -- this must retract a stale observation")
		assert.Zero(t, res.HTTPStatus, "HTTPStatus stays the FAILED-call carrier; a 200 is never stamped there")
	})

	t.Run("successful call reports the origin as authenticated", func(t *testing.T) {
		srv := mcptest.NewCallServer(mcptest.CallServerOpts{
			Tools: map[string]mcptest.ToolHandler{
				"search_issues": mcptest.StaticTool(mcptest.TextResult("2 issues found", false)),
			},
		})
		t.Cleanup(srv.Close)

		tools := mustSynthesize(t, srv.URL)
		_, opID, sess := newOpAndSess(t)
		res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
		require.NoError(t, err, "Execute")

		require.False(t, res.IsError)
		assert.True(t, res.OriginAuthenticated)
	})

	t.Run("upstream 401 does NOT report the origin as authenticated", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		}))
		t.Cleanup(srv.Close)

		tools := mustSynthesize(t, srv.URL)
		_, opID, sess := newOpAndSess(t)
		res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
		require.NoError(t, err, "Execute")

		require.True(t, res.IsError)
		assert.False(t, res.OriginAuthenticated,
			"nothing completed, so nothing may claim the credential was accepted")
		assert.Equal(t, http.StatusUnauthorized, res.HTTPStatus)
	})
}

// TestMCPTool_Execute_HTTPStatusNeverLeaksIntoContent pins the "out-of-band,
// not LLM-facing" half of the contract separately from the value-correctness
// test above: Content (what the model sees) must be unaffected by this
// field's existence — it stays the same "mcp: probe: HTTP 401: ..." text the
// dispatcher already produced before Result.HTTPStatus existed.
func TestMCPTool_Execute_HTTPStatusNeverLeaksIntoContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	t.Cleanup(srv.Close)

	tools := mustSynthesize(t, srv.URL)
	_, opID, sess := newOpAndSess(t)
	res, err := tools[0].Execute(context.Background(), execEnvelope(t, opID, nil), sess)
	require.NoError(t, err, "Execute")
	require.True(t, res.IsError)

	assert.Equal(t, http.StatusUnauthorized, res.HTTPStatus, "the structured field must carry the status")
	assert.Contains(t, res.Content, "HTTP 401", "Content keeps its pre-existing human-readable shape")
	assert.NotContains(t, res.Content, "HTTPStatus", "the struct field name must never appear in LLM-facing text")
}
