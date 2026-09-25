package toolscmd

import (
	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/toolscli"
)

func newToolsGetCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get <name>",
		Aliases: []string{"show"},
		Short:   "Show the detail view of a tool resource (any kind)",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, ns, err := toolsListClientFactory(g)
			if err != nil {
				return err
			}
			m, err := toolscli.Resolve(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			return toolscli.RenderDetail(out, g.Theme(out), m.Kind.Detail(m.Obj))
		},
	}
	return cmd
}
