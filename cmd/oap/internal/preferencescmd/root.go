// Package preferencescmd implements `oap preferences`: reading the resolved
// per-user preferences snapshot a session's agent sees.
package preferencescmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap preferences` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preferences",
		Short: "Inspect a session's resolved per-user preferences",
	}
	cmd.AddCommand(newInspectCmd(g))
	return cmd
}
