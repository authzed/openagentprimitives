package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func newUserIdentityApplyCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewApplyCmd(g, "Server-side apply a UserIdentity YAML")
}
