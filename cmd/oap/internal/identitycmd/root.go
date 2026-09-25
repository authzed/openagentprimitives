// Package identitycmd implements the three identity command trees:
// `oap identity` (an agent's AgentIdentity and the credential-setup flows
// behind it), `oap user-identity` (a person's own credential catalog), and
// `oap idp` (the cluster's identity provider).
package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

func NewCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "identity",
		Aliases: []string{"id"},
		Short:   "Manage AgentIdentity resources",
	}
	cmd.AddCommand(newIdentityListCmd(g))
	cmd.AddCommand(newIdentityShowCmd(g))
	cmd.AddCommand(newIdentityApplyCmd(g))
	cmd.AddCommand(newIdentityDeleteCmd(g))
	cmd.AddCommand(newIdentityPutTokenCmd(g))
	cmd.AddCommand(newIdentityCanonicalIDCmd(g))
	cmd.AddCommand(newIdentityRefreshCmd(g))
	cmd.AddCommand(newIdentitySetupCmd(g))
	return cmd
}
