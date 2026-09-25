package identitycmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/x/externalurl"

	corev1 "k8s.io/api/core/v1"
)

func newIdpRemoveCmd(g *apcmd.Globals) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove the cluster identity provider and its client secret",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runIdpRemove(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), b.Controller, yes)
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip confirmation prompt")
	return cmd
}

func runIdpRemove(ctx context.Context, stdin io.Reader, stdout io.Writer, c client.Client, yes bool) error {
	// Read the CR first so we know the secret name.
	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cidp)
	if apierrors.IsNotFound(err) {
		fmt.Fprintln(stdout, "No cluster identity provider configured; nothing to remove.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ClusterIdentityProvider: %w", err)
	}

	secretName := cidp.Spec.ClientSecretRef.Name
	secretNS := cidp.Spec.ClientSecretRef.Namespace
	if secretNS == "" {
		secretNS = externalurl.Namespace
	}

	if !yes {
		fmt.Fprint(stdout, "Remove the cluster identity provider and its client secret? [y/N]: ")
		buf := make([]byte, 64)
		n, _ := stdin.Read(buf)
		answer := strings.TrimSpace(string(buf[:n]))
		if strings.ToLower(answer) != "y" && strings.ToLower(answer) != "yes" {
			fmt.Fprintln(stdout, "Aborted.")
			return nil
		}
	}

	// Delete CR.
	if err := c.Delete(ctx, &spiceboxv1alpha1.ClusterIdentityProvider{
		ObjectMeta: metav1.ObjectMeta{Name: spiceboxv1alpha1.ClusterIdentityProviderName},
	}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete ClusterIdentityProvider: %w", err)
	}

	// Delete Secret.
	if secretName != "" {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: secretNS},
		}
		if err := c.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete secret %q: %w", secretName, err)
		}
	}

	fmt.Fprintln(stdout, "Identity provider removed.")
	return nil
}
