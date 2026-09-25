// Package sessioncmd implements `oap session`: inspecting AgentSessions and
// their operations, streaming their logs, and the grant / approve / revoke /
// participants verbs over a session's authorization.
package sessioncmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Inspect AgentSession resources",
	}
	cmd.AddCommand(newSessionShowCmd(g))
	cmd.AddCommand(newSessionLogsCmd(g))
	cmd.AddCommand(newSessionOperationsCmd(g))
	cmd.AddCommand(newSessionDeleteCmd(g))
	cmd.AddCommand(newSessionGrantCmd(g))
	cmd.AddCommand(newSessionUnDenyCmd(g))
	cmd.AddCommand(newSessionSlotsCmd(g))
	cmd.AddCommand(newSessionCapabilitiesCmd(g))
	cmd.AddCommand(newSessionRevokeSlotCmd(g))
	cmd.AddCommand(newSessionApproveCmd(g))
	cmd.AddCommand(newSessionRevokeCmd(g))
	cmd.AddCommand(newSessionParticipantsCmd(g))
	cmd.AddCommand(newSessionHoldCmd(g))
	cmd.AddCommand(newSessionCaptureCmd(g))
	return cmd
}
