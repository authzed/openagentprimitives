package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newUserIdentityShowCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of a UserIdentity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var ui spiceboxv1alpha1.UserIdentity
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Name: args[0]}, &ui); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Name:         %s\n", ui.Name)
			fmt.Fprintf(out, "Subject:      %s\n", ui.Spec.Subject)
			fmt.Fprintf(out, "DisplayName:  %s\n", ui.Spec.DisplayName)
			fmt.Fprintln(out, "Credentials:")
			for _, c := range ui.Spec.Credentials {
				printCredentialRow(out, credentialRow(c))
			}
			fmt.Fprintf(out, "AvailableCredentials: %v\n", ui.Status.AvailableCredentials)
			fmt.Fprintln(out, "Conditions:")
			uihelpers.PrintConditions(out, ui.Status.Conditions, "  ")
			return nil
		},
	}
}
