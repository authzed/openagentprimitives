package classcmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newClassListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List SpiceboxClass CRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.SpiceboxClassList
			if err := b.Controller.List(cmd.Context(), &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no SpiceboxClasses installed")
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "VALID", "REASON", "IMAGE", "TOOLS", "AGE")
			anyInvalid := false
			for _, c := range list.Items {
				valid, reason := "Unknown", ""
				for _, cond := range c.Status.Conditions {
					if cond.Type == "Valid" {
						valid = string(cond.Status)
						if cond.Status != metav1.ConditionTrue {
							reason = cond.Reason
							anyInvalid = true
						}
					}
				}
				toolNames := make([]string, 0, len(c.Spec.Tools))
				for _, tl := range c.Spec.Tools {
					toolNames = append(toolNames, tl.Name)
				}
				t.Row(c.Name, valid, reason, c.Spec.Image, joinShort(toolNames, 3), apcmd.DurationSinceShort(c.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			if anyInvalid {
				fmt.Fprintln(out, "\nFor full validation messages, run: oap class show <name>")
			}
			return nil
		},
	}
}

func joinShort(items []string, max int) string {
	if len(items) == 0 {
		return ""
	}
	if len(items) <= max {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:max], ", ") + fmt.Sprintf(" (+%d)", len(items)-max)
}
