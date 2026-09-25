package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newIdentityShowCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show details of an AgentIdentity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			var ai spiceboxv1alpha1.AgentIdentity
			if err := b.Controller.Get(cmd.Context(), client.ObjectKey{Namespace: b.Namespace, Name: args[0]}, &ai); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Name:        %s\n", ai.Name)
			fmt.Fprintf(out, "Namespace:   %s\n", ai.Namespace)
			fmt.Fprintf(out, "Description: %s\n", ai.Spec.Description)
			fmt.Fprintln(out, "Credentials:")
			for _, c := range ai.Spec.Credentials {
				printCredentialRow(out, credentialRow(c))
			}
			fmt.Fprintln(out, "Conditions:")
			uihelpers.PrintConditions(out, ai.Status.Conditions, "  ")
			return nil
		},
	}
}
