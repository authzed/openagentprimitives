// Package classcmd implements `oap class`: inspecting SpiceboxClass resources,
// the sandbox image plus capability descriptor an agent runs against.
package classcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap class` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "class",
		Short: "Inspect SpiceboxClass resources (sandbox image + capability descriptors)",
	}
	cmd.AddCommand(newClassListCmd(g))
	cmd.AddCommand(newClassShowCmd(g))
	return cmd
}
