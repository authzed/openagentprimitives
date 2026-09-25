// Package kgcmd implements `oap kg`, the knowledge-graph query commands.
package kgcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap kg` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kg",
		Short: "Query the knowledge graph",
	}
	cmd.AddCommand(newKGSearchCmd(g))
	cmd.AddCommand(newKGEntityCmd(g))
	cmd.AddCommand(newKGFactsCmd(g))
	cmd.AddCommand(newKGRelatedCmd(g))
	cmd.AddCommand(newKGCommunitiesCmd(g))
	return cmd
}
