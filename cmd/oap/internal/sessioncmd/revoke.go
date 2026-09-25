package sessioncmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
)

func newSessionRevokeCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <session> <subject>",
		Short: "Remove agentsession#participant@<subject> directly (admin override)",
		Long: `Revoke a participant from the named AgentSession.

<subject> may be either:
  * A subject-set expression: "<type>:<id>#<relation>" (e.g. "group:engineering#member")
  * A direct user reference: "user:<canonicalID>" (use 'oap identity canonical-id <email>' to derive)

Bypasses the permission_request flow — useful for programmatic provisioning.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName := args[0]
			subject := args[1]

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, err := apspicedb.NewClientFromEnv()
			if err != nil {
				return err
			}
			defer cl.Close()

			if err := cl.DeleteInteractParticipant(cmd.Context(), b.Namespace, sessionName, subject); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s from agentsession:%s/%s\n", subject, b.Namespace, sessionName)
			return nil
		},
	}
}
