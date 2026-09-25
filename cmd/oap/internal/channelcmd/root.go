// Package channelcmd implements `oap channel`: creating, inspecting and
// watching the Channel CRs that bind an agent to a transport.
package channelcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "channel",
		Aliases: []string{"channels", "ch"},
		Short:   "Manage Channel CRs (Slack, future webhook/cron, ...).",
	}
	cmd.AddCommand(
		newChannelListCmd(g),
		newChannelShowCmd(g),
		newChannelApplyCmd(g),
		newChannelDeleteCmd(g),
		newChannelDiagCmd(g),
		newChannelCreateCmd(g),
	)
	return cmd
}
