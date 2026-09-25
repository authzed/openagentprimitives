package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewUserIdentityCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "user-identity",
		Aliases: []string{"uid"},
		Short:   "Manage UserIdentity resources (per-user credential catalogs)",
	}
	cmd.AddCommand(newUserIdentityApplyCmd(g))
	cmd.AddCommand(newUserIdentityListCmd(g))
	cmd.AddCommand(newUserIdentityShowCmd(g))
	cmd.AddCommand(newUserIdentityPutTokenCmd(g))
	cmd.AddCommand(newUserIdentityDeleteTokenCmd(g))
	cmd.AddCommand(newUserIdentityDeleteCmd(g))
	return cmd
}
