package sessioncmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	apspicedb "github.com/authzed/openagentprimitives/cmd/oap/internal/spicedb"
)

// newSessionUnDenyCmd lifts a Deny. Without it a Deny is permanent for the life
// of the session: the blocklist had two writers (a Deny click and the fork
// carry-over) and no delete, so a misclick had no remedy.
func newSessionUnDenyCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "un-deny <session> <subject>",
		Short: "Remove agentsession#denied@<subject> (lift a Deny)",
		Long: `Lift a Deny on the named AgentSession.

<subject> is either a direct user reference — "user:<canonicalID>", from
'oap identity canonical-id <email>' — or a subject set such as
"group:eng#member". Both are valid: the blocklist accepts every subject type
owner and participant do, because a grant that can be made to a group must be
revocable from that same group.

Denying subtracts from every permission the session grants — interact, approve,
fork, and manage_scope — so lifting it restores whatever standing the subject
had before. It does NOT grant standing they never had.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionName, subject := args[0], args[1]

			if !strings.Contains(subject, ":") {
				return fmt.Errorf("subject %q: must be \"user:<canonicalID>\" or a subject set such as \"group:eng#member\"", subject)
			}

			b, err := g.Bundle()
			if err != nil {
				return err
			}
			cl, err := apspicedb.NewClientFromEnv()
			if err != nil {
				return err
			}
			defer cl.Close()

			if err := cl.DeleteDenied(cmd.Context(), b.Namespace, sessionName, subject); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "lifted deny for %s on agentsession:%s/%s\n", subject, b.Namespace, sessionName)
			return nil
		},
	}
}
