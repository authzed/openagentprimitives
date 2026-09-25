package toolscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newToolkitCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "toolkit",
		Short: "Inspect SpiceboxToolkit resources (registered tool descriptions)",
	}
	cmd.AddCommand(newToolkitListCmd(g))
	cmd.AddCommand(newToolkitShowCmd(g))
	return cmd
}
