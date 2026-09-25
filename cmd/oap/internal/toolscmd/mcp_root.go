package toolscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// newMCPCmd hosts the MCPServer-spec authoring helpers, mirroring the
// `oap tools toolspec` set. Verbs: check, explain, test, probe.
func newMCPCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "MCPServer-spec authoring helpers (check, explain, test, probe)",
	}
	cmd.AddCommand(newMCPCheckCmd())
	cmd.AddCommand(newMCPExplainCmd())
	cmd.AddCommand(newMCPTestCmd())
	cmd.AddCommand(newMCPProbeCmd(g))
	return cmd
}
