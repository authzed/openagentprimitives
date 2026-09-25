package sessioncmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
)

func newSessionParticipantsCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "participants <session>",
		Short: "List users with interact permission on an AgentSession via SpiceDB",
		Long: `List all users that currently have interact permission on the named AgentSession.

Calls SpiceDB's LookupSubjects API for agentsession#interact. Requires
SPICEDB_ENDPOINT and SPICEDB_TOKEN to be set.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName := args[0]

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, err := apspicedb.NewClientFromEnv()
			if err != nil {
				return err
			}
			defer cl.Close()

			subjects, err := cl.LookupInteractSubjects(cmd.Context(), b.Namespace, sessionName)
			if err != nil {
				return err
			}
			for _, s := range subjects {
				fmt.Fprintln(cmd.OutOrStdout(), s)
			}
			return nil
		},
	}
}
