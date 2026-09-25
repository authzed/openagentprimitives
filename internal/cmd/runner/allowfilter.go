package main

import (
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// mcpServerAllowed reports whether the MCP server identified by serverCRName
// (the MCPServer CR name, matching AllowedMCPServer.Name as set by the
// admission gate in pkg/platform/settings/resolve.go) may be loaded under the effective
// settings. Tri-state semantics on eff.AllowedMCP:
//
//   - nil eff, or nil/absent AllowedMCP → unconstrained → true
//   - non-nil empty AllowedMCP → allow-none → false
//   - non-nil non-empty AllowedMCP → true iff serverCRName is listed
func mcpServerAllowed(eff *spiceboxv1alpha1.EffectiveSettings, serverCRName string) bool {
	if eff == nil {
		return true
	}
	if eff.AllowedMCP == nil {
		// nil slice ≡ no tier constrained MCP servers → unconstrained
		return true
	}
	for _, a := range eff.AllowedMCP {
		if a.Name == serverCRName {
			return true
		}
	}
	return false
}
