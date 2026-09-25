package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newIdentityListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List AgentIdentity CRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.AgentIdentityList
			if err := b.Controller.List(cmd.Context(), &list, client.InNamespace(b.Namespace)); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no AgentIdentity CRs")
				return nil
			}
			// CREDS is deliberately a count: an AgentIdentity's credentials are
			// secret material, and a list a user pipes or pastes must carry
			// none of it. Capabilities come from the stream this command writes
			// to, so a pipe, --no-color and NO_COLOR each land on the colorless
			// theme.
			t := g.Table(out,
				"NAME", "NAMESPACE", "VALID", "CREDS", "AGE")
			for _, ai := range list.Items {
				valid := "Unknown"
				for _, c := range ai.Status.Conditions {
					if c.Type == spiceboxv1alpha1.AgentIdentityConditionValid {
						valid = string(c.Status)
					}
				}
				t.Row(ai.Name, ai.Namespace, valid,
					fmt.Sprintf("%d", len(ai.Spec.Credentials)),
					apcmd.DurationSinceShort(ai.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
}
