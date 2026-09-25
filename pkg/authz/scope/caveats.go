package scope

import "fmt"

// CaveatCategory tags a known completeness gap in a hard-deny.
// Set by deterministic detection in DetectCaveats; LLM composer
// fills the Explanation downstream.
type CaveatCategory string

const (
	CaveatSearchToolCanReturn        CaveatCategory = "search_tool_can_return"
	CaveatListToolReturnsParentTaint CaveatCategory = "list_tool_returns_parent_taint"
	CaveatPatternNotEnumerable       CaveatCategory = "pattern_not_enumerable"
	CaveatAliasOrAltID               CaveatCategory = "alias_or_alt_id"
	CaveatHostVsAddress              CaveatCategory = "host_vs_address"
	CaveatMethodOrActionNeighbors    CaveatCategory = "method_or_action_neighbors"
)

// CaveatItem is one applied-but-incomplete fragment of the delta.
// AppliedFragment is a short, deterministic description; Explanation
// is LLM-composed; Category drives template selection in the composer's
// system prompt.
type CaveatItem struct {
	// AppliedFragment is the part of the applied delta this caveat qualifies.
	AppliedFragment string `json:"appliedFragment"`
	// Category selects the composer's explanation template; always set.
	Category CaveatCategory `json:"category"`
	// Explanation is LLM-composed prose; empty until the composer fills it.
	Explanation string `json:"explanation,omitempty"`
}

// ToolReads is the per-tool reads declaration from MCPServer
// toolResourceMap, projected into the shape DetectCaveats needs.
type ToolReads struct {
	ResourceType string
	IDArg        string // empty when QueryStyle = true
	QueryStyle   bool   // true for search/list tools that don't take a specific ID
	ParentType   string // non-empty when this tool taints under a parent type
}

// DetectCaveats inspects the applied delta's hard-denies and envelope
// tooling to flag known incompleteness gaps. Pure; no side effects.
func DetectCaveats(d ScopeDelta, env AgentClassEnvelope, toolReads map[string]ToolReads) []CaveatItem {
	var out []CaveatItem

	for _, ref := range d.HardDeny.Resources {
		for _, et := range env.Tools {
			tr, ok := toolReads[et.Name]
			if !ok || tr.ResourceType != ref.ResourceType {
				continue
			}
			switch {
			case tr.QueryStyle:
				out = append(out, CaveatItem{
					AppliedFragment: fmt.Sprintf("disallow read of %s", ref.String()),
					Category:        CaveatSearchToolCanReturn,
				})
			case tr.ParentType != "":
				out = append(out, CaveatItem{
					AppliedFragment: fmt.Sprintf("disallow read of %s", ref.String()),
					Category:        CaveatListToolReturnsParentTaint,
				})
			}
		}
	}

	for _, pat := range d.HardDeny.ResourcePatterns {
		out = append(out, CaveatItem{
			AppliedFragment: fmt.Sprintf("disallow matching pattern %v", pat.Attrs),
			Category:        CaveatPatternNotEnumerable,
		})
	}

	return out
}
