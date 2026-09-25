// Package toolscmd implements `oap tools`: the kind-agnostic verbs over every
// registered tool kind (list, get, edit, apply, validate, lint, probe, gen),
// plus the per-kind authoring sub-trees — toolspec, mcp — and the toolkit and
// toolcall resource commands.
package toolscmd

import (
	"github.com/spf13/cobra"

	// Side-effect imports: each kind's init() registers itself with
	// pkg/tools/kinds/registry so the verbs below can dispatch.
	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/mcp"
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/sandbox"
	_ "github.com/authzed/openagentprimitives/pkg/tools/kinds/sidecartoolbox"
)

// NewCmd is the parent for `oap tools <verb>`. The verbs are kind-agnostic
// — they consult pkg/tools/kinds/registry to dispatch to the right Kind impl.
//
// Adding a new tool kind is purely additive: drop a package under
// pkg/tools/kinds/<x>/ that calls registry.Register from its init(); no CLI
// edits required.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools",
		Short: "Manage tools used by agents (kind-agnostic)",
	}
	cmd.AddCommand(newToolsListCmd(g))
	cmd.AddCommand(newToolsGetCmd(g))
	cmd.AddCommand(newToolsEditCmd(g))
	cmd.AddCommand(newToolsApplyCmd(g))
	cmd.AddCommand(newToolsValidateCmd(g))
	cmd.AddCommand(newToolsLintCmd(g))
	cmd.AddCommand(newToolsProbeCmd(g))
	// Kind-agnostic gen lives at `oap tools gen`; it consults the
	// unified catalog (sandbox toolkits + MCP server registry) and
	// dispatches to the kind-specific authoring loop.
	cmd.AddCommand(newToolsGenCmd(g))
	// Remaining sandbox-only authoring helpers (explain/check/describe/test)
	// stay under the toolspec sub-tree.
	cmd.AddCommand(newToolspecCmd(g))
	// MCP-spec authoring helpers (check today; explain/test in follow-ups)
	// mirror the toolspec sub-tree for MCPServer specs.
	cmd.AddCommand(newMCPCmd(g))
	// Toolkit and Toolcall management commands are orthogonal to the
	// tool-kind unification — they manage SpiceboxToolkit and ToolCall CRs,
	// which are not authored as files. They stay under `oap tools toolkit`
	// and `oap tools toolcall`.
	cmd.AddCommand(newToolkitCmd(g))
	cmd.AddCommand(newToolcallCmd(g))
	return cmd
}
