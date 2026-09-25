package agentcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newAgentApplyCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewApplyCmd(g, "Server-side apply an AgentClass YAML")
}
