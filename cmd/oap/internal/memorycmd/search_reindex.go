package memorycmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/memclient"
)

func newSearchReindexCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reindex <session>",
		Short: "Rebuild search indexes for a session's entries",
		Long: `Triggers a server-side reindex of all memory entries in the given
session into search providers. Use after adding a new search provider
to an existing deployment, or to recover from index corruption.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSearchReindex(cmd, g, args[0])
		},
	}
	return cmd
}

func runSearchReindex(cmd *cobra.Command, g *apcmd.Globals, sessionName string) error {
	ctx := cmd.Context()
	b, err := g.Bundle()
	if err != nil {
		return err
	}
	conn, err := memclient.Connect(ctx, b, sessionName, memoryBackend(cmd))
	if err != nil {
		return err
	}
	defer conn.Close()

	result, err := conn.Client.Reindex(ctx, conn.Scope)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Reindexed %d entries in %s\n", result.Count, sessionName)
	return nil
}
