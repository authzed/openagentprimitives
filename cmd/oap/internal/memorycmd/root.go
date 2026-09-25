// Package memorycmd implements `oap memory`: inspecting, querying, searching
// and sharing the entries in a session's memory scopes.
package memorycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// NewCmd builds the `oap memory` subtree.
func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "memory",
		Aliases: []string{"mem"},
		Short:   "Inspect and manage session memory entries",
	}
	cmd.PersistentFlags().String("backend", "", `Read from "primary" (inmem) or "secondary" (postgres) when shadow mode is active`)
	cmd.AddCommand(newMemoryListCmd(g))
	cmd.AddCommand(newMemoryGetCmd(g))
	cmd.AddCommand(newMemoryQueryCmd(g))
	cmd.AddCommand(newMemorySearchCmd(g))
	cmd.AddCommand(newMemoryPutCmd(g))
	cmd.AddCommand(newMemoryDeleteCmd(g))
	cmd.AddCommand(newMemoryKindsCmd(g))
	cmd.AddCommand(newMemoryShareCmd(g))
	cmd.AddCommand(newSearchReindexCmd(g))
	return cmd
}

func memoryBackend(cmd *cobra.Command) string {
	v, _ := cmd.Flags().GetString("backend")
	return v
}
