package toolscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newToolcallCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "toolcall",
		Short: "Inspect ToolCall resources (per-invocation sandbox tool exec records)",
	}
	cmd.AddCommand(newToolcallListCmd(g))
	cmd.AddCommand(newToolcallShowCmd(g))
	return cmd
}
