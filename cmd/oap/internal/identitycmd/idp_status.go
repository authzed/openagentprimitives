package identitycmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/clilogin"
	"github.com/authzed/openagentprimitives/cmd/oap/internal/uihelpers"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

func newIdpStatusCmd(g *apcmd.Globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the cluster identity provider status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runIdpStatus(cmd.Context(), cmd.OutOrStdout(), b.Controller)
		},
	}
}

func runIdpStatus(ctx context.Context, stdout io.Writer, c client.Client) error {
	var cidp spiceboxv1alpha1.ClusterIdentityProvider
	err := c.Get(ctx, types.NamespacedName{Name: spiceboxv1alpha1.ClusterIdentityProviderName}, &cidp)
	if apierrors.IsNotFound(err) {
		fmt.Fprintln(stdout, "No cluster identity provider configured. Run `oap idp setup <kind>`.")
		return nil
	}
	if err != nil {
		return fmt.Errorf("get ClusterIdentityProvider: %w", err)
	}

	spec := cidp.Spec
	fmt.Fprintf(stdout, "Kind:       %s\n", spec.Kind)
	fmt.Fprintf(stdout, "Client ID:  %s\n", spec.ClientID)
	if spec.Issuer != "" {
		fmt.Fprintf(stdout, "Issuer:     %s\n", spec.Issuer)
	}
	if len(spec.AllowedEmailDomains) > 0 {
		fmt.Fprintf(stdout, "Domains:    %v\n", spec.AllowedEmailDomains)
	} else if spec.AllowAnyEmail {
		fmt.Fprintln(stdout, "Domains:    (any)")
	} else {
		fmt.Fprintln(stdout, "Domains:    (none — fail closed)")
	}
	if spec.SessionTTL != nil {
		fmt.Fprintf(stdout, "Session TTL: %s\n", spec.SessionTTL.Duration)
	}

	// Best-effort callback URL.
	cbURL := "unknown (no external URL)"
	if baseURL, berr := clilogin.ResolveIdentitydBaseURL(ctx, c); berr == nil {
		cbURL = baseURL + "/oidc/callback/idp"
	}
	fmt.Fprintf(stdout, "Callback URL: %s\n", cbURL)

	fmt.Fprintln(stdout, "")
	fmt.Fprintln(stdout, "Conditions:")
	uihelpers.PrintConditions(stdout, cidp.Status.Conditions, "  ")
	return nil
}
