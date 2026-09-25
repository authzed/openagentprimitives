package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewIdpCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "idp",
		Short: "Manage the cluster identity provider",
	}
	cmd.AddCommand(newIdpSetupCmd(g))
	cmd.AddCommand(newIdpStatusCmd(g))
	cmd.AddCommand(newIdpRemoveCmd(g))
	return cmd
}
