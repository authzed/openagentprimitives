package identitycmd

import (
	"fmt"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func newUserIdentityDeleteCmd(g *apcmd.Globals) *cobra.Command {
	var cascadeSecrets bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a UserIdentity",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()

			// If --cascade-secrets is set, collect the credential list before
			// deleting the UserIdentity so we know which Secrets to remove.
			var secretNames []string
			if cascadeSecrets {
				var ui spiceboxv1alpha1.UserIdentity
				if err := b.Controller.Get(ctx, client.ObjectKey{Name: args[0]}, &ui); err != nil {
					if !apierrors.IsNotFound(err) {
						return fmt.Errorf("get UserIdentity: %w", err)
					}
				} else {
					for _, c := range ui.Spec.Credentials {
						secretNames = append(secretNames, useridentity.MasterSecretName(ui.Name, c.Name))
					}
				}
			}

			obj := &spiceboxv1alpha1.UserIdentity{}
			obj.Name = args[0]
			if err := b.Controller.Delete(ctx, obj); err != nil {
				return err
			}
			fmt.Fprintf(out, "deleted UserIdentity %s\n", args[0])

			for _, secName := range secretNames {
				secObj := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      secName,
						Namespace: spiceboxv1alpha1.IdentitiesNamespace,
					},
				}
				if err := b.Controller.Delete(ctx, secObj); err != nil {
					if apierrors.IsNotFound(err) {
						fmt.Fprintf(out, "  secret %s/%s already absent\n", spiceboxv1alpha1.IdentitiesNamespace, secName)
					} else {
						fmt.Fprintf(out, "  warning: delete secret %s/%s: %v\n", spiceboxv1alpha1.IdentitiesNamespace, secName, err)
					}
				} else {
					fmt.Fprintf(out, "  deleted secret %s/%s\n", spiceboxv1alpha1.IdentitiesNamespace, secName)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&cascadeSecrets, "cascade-secrets", false,
		"Also delete each credential's master Secret in the identities namespace (default: leave Secrets intact)")
	return cmd
}
