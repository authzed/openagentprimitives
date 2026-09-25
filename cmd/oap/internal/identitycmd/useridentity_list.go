package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newUserIdentityListCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List UserIdentity CRs",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var list spiceboxv1alpha1.UserIdentityList
			if err := b.Controller.List(cmd.Context(), &list); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list.Items) == 0 {
				fmt.Fprintln(out, "no UserIdentity CRs")
				return nil
			}
			// Capabilities come from the stream this command writes to, so a
			// pipe, --no-color and NO_COLOR each land on the colorless theme.
			//
			// CREDS is the count of the identity's named credentials, never
			// their values: the spec holds secretRefs, and the row must stay
			// safe to paste into an issue.
			t := g.Table(out,
				"NAME", "SUBJECT", "VALID", "CREDS", "AGE")
			for _, ui := range list.Items {
				valid := "Unknown"
				for _, c := range ui.Status.Conditions {
					if c.Type == spiceboxv1alpha1.UserIdentityConditionValid {
						valid = string(c.Status)
					}
				}
				t.Row(ui.Name, ui.Spec.Subject, valid,
					fmt.Sprintf("%d", len(ui.Spec.Credentials)),
					apcmd.DurationSinceShort(ui.CreationTimestamp.Time))
			}
			fmt.Fprint(out, t.Render())
			return nil
		},
	}
}
