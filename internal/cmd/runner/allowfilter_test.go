package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// helpers

func effWithMCP(servers []spiceboxv1alpha1.AllowedMCPServer) *spiceboxv1alpha1.EffectiveSettings {
	return &spiceboxv1alpha1.EffectiveSettings{AllowedMCP: servers}
}

func TestMCPServerAllowed(t *testing.T) {
	// The key passed to mcpServerAllowed is the MCPServer CR name (ref.Ref),
	// NOT the AgentClass.spec.mcpServers[i].name LLM-prefix (ref.Name). Test
	// names are kept deliberately distinct from plausible LLM prefixes to make
	// this obvious: a revert to ref.Name would break the match cases.
	cases := []struct {
		name         string
		eff          *spiceboxv1alpha1.EffectiveSettings
		serverCRName string
		want         bool
	}{
		// nil eff → unconstrained → allow all
		{
			name:         "nil eff: unconstrained → allow all",
			eff:          nil,
			serverCRName: "mcp-github-cr",
			want:         true,
		},
		// nil AllowedMCP (zero-value EffectiveSettings) → unconstrained → allow all
		{
			name:         "nil AllowedMCP: unconstrained → allow all",
			eff:          &spiceboxv1alpha1.EffectiveSettings{},
			serverCRName: "mcp-github-cr",
			want:         true,
		},
		// empty (non-nil) AllowedMCP → allow none
		{
			name:         "empty AllowedMCP: allow none",
			eff:          effWithMCP([]spiceboxv1alpha1.AllowedMCPServer{}),
			serverCRName: "mcp-github-cr",
			want:         false,
		},
		// non-empty AllowedMCP containing the CR name → allow
		{
			name: "non-empty AllowedMCP containing CR name: allow",
			eff: effWithMCP([]spiceboxv1alpha1.AllowedMCPServer{
				{Name: "mcp-github-cr", Tools: []string{"*"}},
				{Name: "mcp-jira-cr"},
			}),
			serverCRName: "mcp-github-cr",
			want:         true,
		},
		// non-empty AllowedMCP not containing the CR name → deny
		{
			name: "non-empty AllowedMCP not containing CR name: deny",
			eff: effWithMCP([]spiceboxv1alpha1.AllowedMCPServer{
				{Name: "mcp-jira-cr"},
			}),
			serverCRName: "mcp-github-cr",
			want:         false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mcpServerAllowed(tc.eff, tc.serverCRName))
		})
	}
}
