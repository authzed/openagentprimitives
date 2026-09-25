package toolscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
)

// newToolspecCmd hosts the sandbox-toolspec authoring helpers that
// don't yet have parallels in other tool kinds. The cluster + file
// verbs (validate / lint / probe / list / show / apply) moved to
// `oap tools <verb>` in the kind-agnostic CLI; gen moved to
// `oap tools gen`.
func newToolspecCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "toolspec",
		Short: "Sandbox-toolspec authoring helpers (gen moved to oap tools gen; cluster verbs moved to oap tools <verb>)",
	}
	cmd.AddCommand(newToolspecExplainCmd())
	cmd.AddCommand(newToolspecCheckCmd())
	cmd.AddCommand(newToolspecDescribeCmd())
	cmd.AddCommand(newToolspecTestCmd())
	return cmd
}
