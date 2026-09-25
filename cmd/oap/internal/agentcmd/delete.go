package agentcmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentDeleteCmd(g *apcmd.Globals) *cobra.Command {
	return apcmd.NewNamespacedDeleteCmd(g, "Delete an AgentClass", "AgentClass",
		func() *spiceboxv1alpha1.AgentClass { return &spiceboxv1alpha1.AgentClass{} })
}
