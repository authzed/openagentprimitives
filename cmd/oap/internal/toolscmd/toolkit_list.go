package toolscmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newToolkitListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List SpiceboxToolkit CRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.SpiceboxToolkitList
			if err := b.Controller.List(cmd.Context(), &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no SpiceboxToolkits installed (builtin toolkits are not stored as CRs)")
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "VALID", "REVISION", "SUBCMDS", "AGE")
			for _, tk := range list.Items {
				valid := "Unknown"
				for _, c := range tk.Status.Conditions {
					if c.Type == "Valid" {
						valid = string(c.Status)
					}
				}
				t.Row(tk.Name, valid, tk.Spec.ToolkitRevision,
					fmt.Sprintf("%d", len(tk.Spec.Subcommands)),
					apcmd.DurationSinceShort(tk.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
}
