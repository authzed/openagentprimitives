package installcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewSpiceDBCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spicedb",
		Short: "Apply schema and check permissions against the system SpiceDB.",
	}
	cmd.AddCommand(newSpiceDBApplySchemaCmd(g))
	cmd.AddCommand(newSpiceDBCheckCmd(g))
	cmd.AddCommand(newSpiceDBProxyCmd(g))
	cmd.AddCommand(newSpiceDBExposeCmd(g))
	return cmd
}
