package identitycmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newIdentityDeleteCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewNamespacedDeleteCmd(g, "Delete an AgentIdentity", "AgentIdentity",
		func() *spiceboxv1alpha1.AgentIdentity { return &spiceboxv1alpha1.AgentIdentity{} })
}
