package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newIdentityApplyCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewApplyCmd(g, "Server-side apply an AgentIdentity YAML")
}
