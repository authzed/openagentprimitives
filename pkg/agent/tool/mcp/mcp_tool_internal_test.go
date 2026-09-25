package mcp

import (
	"testing"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/stretchr/testify/assert"
)

func TestMCPToolOrigin(t *testing.T) {
	// Origin keys on the MCPServer CR name (originName), NOT the LLM-prefix
	// (serverName) — set them to distinct values to prove Origin reads the
	// right field. This is the revocation key: revoke publishers emit
	// "mcpserver/"+crName, so Origin must match the CR name or revocation is a
	// silent no-op when the AgentClass LLM-prefix differs from the CR name.
	tl := &MCPTool{serverName: "gh", originName: "mcp-github"}
	var ot agenttool.OriginTool = tl // compile-time interface check
	assert.Equal(t, "mcpserver/mcp-github", ot.Origin())
	assert.NotEqual(t, "mcpserver/gh", ot.Origin(), "Origin must not key on serverName/LLM-prefix")
}

// TestCredentialIdentity pins what the MCP session cache keys on. Its one hard
// requirement is that a credential's identity does NOT move when its value does:
// the operator refreshes OAuth tokens under a live AgentSession, and dispatch's
// reauthPersist writes the refreshed value straight back onto the tool, so a
// value-derived identity would silently drop the cached MCP session — and the
// server-side state it holds — one call after every refresh.
func TestCredentialIdentity(t *testing.T) {
	withGate := func(credID string) *MCPTool {
		return &MCPTool{originName: "mcp-github", useTokenGate: &UseTokenGate{CredID: credID}}
	}
	cases := []struct {
		name  string
		tool  *MCPTool
		value string
		want  string
	}{
		{"credential named by its source: keyed on that source", withGate("abc123"), "Bearer v1", "cred/abc123"},
		{"same source, value rotated by a refresh: identity unchanged", withGate("abc123"), "Bearer v2-refreshed", "cred/abc123"},
		{"different source on the same server: a different identity", withGate("def456"), "Bearer v1", "cred/def456"},
		{"authenticated but source unnamed: falls back to the MCPServer CR", &MCPTool{originName: "mcp-github"}, "Bearer v1", "mcpserver/mcp-github"},
		{"gate wired with no CredID: same CR fallback", &MCPTool{originName: "mcp-github", useTokenGate: &UseTokenGate{}}, "Bearer v1", "mcpserver/mcp-github"},
		{"no credential at all: empty, so anonymous callers share one session", &MCPTool{originName: "mcp-github"}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.tool.credentialIdentity(tc.value))
		})
	}

	t.Run("unnamed credentials never share across MCPServer CRs", func(t *testing.T) {
		a := &MCPTool{originName: "mcp-github"}
		b := &MCPTool{originName: "mcp-linear"}
		assert.NotEqual(t, a.credentialIdentity("Bearer same-bytes"), b.credentialIdentity("Bearer same-bytes"),
			"two CRs we cannot prove share a credential must not share a session's transport")
	})
}
