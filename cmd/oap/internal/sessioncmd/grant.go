package sessioncmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func newSessionGrantCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "grant <session> <subject>",
		Short: "Write agentsession#participant@<subject> directly (admin override)",
		Long: `Grant an additional participant on the named AgentSession.

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

			if _, _, _, perr := spicedb.ParseSubject(subject); perr == nil {
				// subject-set form
				if err := cl.TouchInteractParticipant(cmd.Context(), b.Namespace, sessionName, subject); err != nil {
					return err
				}
			} else if strings.HasPrefix(subject, "user:") {
				// Operator-supplied on the CLI, run under the root SpiceDB key.
				// Nothing verifies who this names.
				canonical := identity.CanonicalFromTrusted(strings.TrimPrefix(subject, "user:"),
					"subject typed on the oap CLI by a root-key holder (unverified; see G5/G7)")
				if err := cl.TouchInteractParticipantUser(cmd.Context(), b.Namespace, sessionName, canonical); err != nil {
					return err
				}
			} else {
				return fmt.Errorf("subject %q: must be \"<type>:<id>#<relation>\" or \"user:<canonicalID>\"", subject)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted %s on agentsession:%s/%s\n", subject, b.Namespace, sessionName)
			return nil
		},
	}
}
