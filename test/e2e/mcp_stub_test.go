//go:build e2e

package e2e_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e"
)

func TestMCPStub_OnTool_RespondsToCall(t *testing.T) {
	m := e2e.NewMCPStub(t)
	m.OnTool("list_companies", func(args map[string]any) any {
		return []map[string]any{{"id": "acme-id", "name": "Acme"}}
	})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_companies","arguments":{"sinceDays":7}}}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	result, ok := got["result"].(map[string]any)
	require.True(t, ok, "response has result: %s", raw)
	content, ok := result["content"].([]any)
	require.True(t, ok)
	require.Len(t, content, 1)
	first := content[0].(map[string]any)
	assert.Equal(t, "text", first["type"])
	assert.Contains(t, first["text"], "acme-id")
}

func TestMCPStub_ToolNotRegistered_ReturnsError(t *testing.T) {
	m := e2e.NewMCPStub(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"unknown","arguments":{}}}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.NotNil(t, got["error"], "unknown tool yields JSON-RPC error: %s", raw)
}

func TestMCPStub_Calls_RecordedForAssertion(t *testing.T) {
	m := e2e.NewMCPStub(t)
	m.OnTool("list_companies", func(args map[string]any) any { return []any{} })
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_companies","arguments":{"sinceDays":14}}}`
	_, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	calls := m.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "list_companies", calls[0].Name)
	assert.Equal(t, float64(14), calls[0].Args["sinceDays"])
}

func TestMCPStub_OnToolError_ReturnsJSONRPCError(t *testing.T) {
	m := e2e.NewMCPStub(t)
	m.OnToolError("flaky", -32000, "transient backend error")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flaky","arguments":{}}}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	errObj, ok := got["error"].(map[string]any)
	require.True(t, ok, "expected error object: %s", raw)
	assert.Equal(t, float64(-32000), errObj["code"])
	assert.Contains(t, errObj["message"], "transient backend error")
}

// TestMCPStub_FailHTTP_InjectsTheChosenStatus covers the three properties a
// scenario depends on and cannot see when they break: the status is the
// CALLER's (a credential scenario needs 401, not FailTransport's 500), a
// counted injection stops after n, and a negative count stays live across the
// several HTTP attempts one logical tool call makes.
func TestMCPStub_FailHTTP_InjectsTheChosenStatus(t *testing.T) {
	call := func(m *e2e.MCPStub, name string) int {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
		resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	t.Run("a counted injection answers the chosen status, then stops", func(t *testing.T) {
		m := e2e.NewMCPStub(t)
		m.OnTool("upstream", func(map[string]any) any { return map[string]any{"ok": true} })
		m.FailHTTP("upstream", http.StatusUnauthorized, 1)
		assert.Equal(t, http.StatusUnauthorized, call(m, "upstream"))
		assert.Equal(t, http.StatusOK, call(m, "upstream"), "the injection is spent after n calls")
	})

	t.Run("a negative count stays live until cleared", func(t *testing.T) {
		m := e2e.NewMCPStub(t)
		m.OnTool("upstream", func(map[string]any) any { return map[string]any{"ok": true} })
		m.FailHTTP("upstream", http.StatusUnauthorized, -1)
		assert.Equal(t, http.StatusUnauthorized, call(m, "upstream"))
		assert.Equal(t, http.StatusUnauthorized, call(m, "upstream"))
		m.FailHTTP("upstream", 0, 0)
		assert.Equal(t, http.StatusOK, call(m, "upstream"), "n == 0 clears the injection")
	})

	t.Run("FailTransport still means 500", func(t *testing.T) {
		m := e2e.NewMCPStub(t)
		m.OnTool("upstream", func(map[string]any) any { return map[string]any{"ok": true} })
		m.FailTransport("upstream", 1)
		assert.Equal(t, http.StatusInternalServerError, call(m, "upstream"),
			"FailTransport's existing callers depend on the 500 it has always written")
	})
}

func TestMCPStub_ToolsList_ReturnsRegisteredTools(t *testing.T) {
	m := e2e.NewMCPStub(t)
	m.OnTool("alpha", func(args map[string]any) any { return nil })
	m.OnTool("beta", func(args map[string]any) any { return nil })

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	result := got["result"].(map[string]any)
	tools := result["tools"].([]any)
	require.Len(t, tools, 2)
	names := map[string]bool{}
	for _, tt := range tools {
		names[tt.(map[string]any)["name"].(string)] = true
	}
	assert.True(t, names["alpha"])
	assert.True(t, names["beta"])
}

func TestMCPStub_Initialize_ReturnsServerInfo(t *testing.T) {
	m := e2e.NewMCPStub(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	result, ok := got["result"].(map[string]any)
	require.True(t, ok, "initialize returns result: %s", raw)
	assert.NotNil(t, result["protocolVersion"])
	assert.NotNil(t, result["capabilities"])
	assert.NotNil(t, result["serverInfo"])
}

func TestMCPStub_OnToolWithMeta_EmitsMetaAnnotations(t *testing.T) {
	m := e2e.NewMCPStub(t)
	m.OnToolWithMeta("flag_me", func(args map[string]any) (any, map[string]any) {
		return map[string]any{"ok": true},
			map[string]any{"maliciousActivityHint": true, "attribution": []string{"mcp://x"}}
	})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"flag_me","arguments":{}}}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var env struct {
		Result struct {
			Meta *struct {
				Annotations *struct {
					MaliciousActivityHint bool     `json:"maliciousActivityHint"`
					Attribution           []string `json:"attribution"`
				} `json:"annotations"`
			} `json:"_meta"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(b, &env))
	require.NotNil(t, env.Result.Meta, "response must have _meta block: %s", b)
	require.NotNil(t, env.Result.Meta.Annotations, "_meta must have annotations block: %s", b)
	assert.True(t, env.Result.Meta.Annotations.MaliciousActivityHint)
	assert.Equal(t, []string{"mcp://x"}, env.Result.Meta.Annotations.Attribution)
}

func TestMCPStub_AnnounceTools_OverridesDefaultList(t *testing.T) {
	m := e2e.NewMCPStub(t)
	// Register handlers; AnnounceTools should still win for tools/list.
	m.OnTool("internal", func(args map[string]any) any { return nil })
	m.AnnounceTools([]map[string]any{{
		"name":        "advertised",
		"description": "richer schema",
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"q": map[string]any{"type": "string"}},
			"required":   []string{"q"},
		},
	}})

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp, err := http.Post(m.URL(), "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	tools := got["result"].(map[string]any)["tools"].([]any)
	require.Len(t, tools, 1, "AnnounceTools overrides synthesized list")
	assert.Equal(t, "advertised", tools[0].(map[string]any)["name"])
	schema := tools[0].(map[string]any)["inputSchema"].(map[string]any)
	assert.Equal(t, "object", schema["type"])
}
