package agentcmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newAgentListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List AgentClass CRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.AgentClassList
			if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no AgentClass CRs")
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			t := g.Table(out,
				"NAME", "NAMESPACE", "VALID", "MODEL", "BUNDLES", "AGE")
			for _, ac := range list.Items {
				valid := "Unknown"
				for _, c := range ac.Status.Conditions {
					if c.Type == spiceboxv1alpha1.AgentClassConditionValid {
						valid = string(c.Status)
					}
				}
				model := "(inherited)"
				if ac.Spec.Model != nil {
					model = fmt.Sprintf("%s/%s", ac.Spec.Model.Provider, ac.Spec.Model.Name)
				}
				bundles := fmt.Sprintf("%d", len(ac.Spec.ToolBundles))
				t.Row(ac.Name, ac.Namespace, valid, model, bundles,
					apcmd.DurationSinceShort(ac.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
}
