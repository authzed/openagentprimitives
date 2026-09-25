package identitycmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	credkindregistry "github.com/authzed/openagentprimitives/pkg/platform/identity/credkind/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/refresh"
)

func newIdentityRefreshCmd(g *apcmd.Globals) *cobra.Command {
	var (
		credName  string
		allOAuth  bool
		threshold time.Duration
	)
	cmd := &cobra.Command{
		Use:   "refresh <identity-name>",
		Short: "Refresh expiring OAuth credentials on an AgentIdentity",
		Long: `Walk AgentIdentity.spec.credentials[*] looking for type=oauth.
For each whose expires_at is within --threshold (default 5m) — or every
oauth credential if --all-oauth — perform an RFC 6749 refresh-token
grant against the stored token_endpoint and update the Secret in place.

--credential <name> refreshes one specific credential (skips threshold check).
Refresh failures don't mutate existing Secret values.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runIdentityRefresh(cmd.Context(), cmd.OutOrStdout(), b.Controller, b.Namespace, args[0],
				credName, allOAuth, threshold)
		},
	}
	cmd.Flags().StringVar(&credName, "credential", "", "Refresh a single credential by name")
	cmd.Flags().BoolVar(&allOAuth, "all-oauth", false, "Refresh every type=oauth credential")
	cmd.Flags().DurationVar(&threshold, "threshold", 5*time.Minute, "Refresh credentials expiring within this duration")
	return cmd
}

func runIdentityRefresh(ctx context.Context, out io.Writer, c client.Client,
	namespace, identityName, credName string, allOAuth bool, threshold time.Duration) error {

	var ai spiceboxv1alpha1.AgentIdentity
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: identityName}, &ai); err != nil {
		return fmt.Errorf("get AgentIdentity %q: %w", identityName, err)
	}

	type plan struct {
		cred spiceboxv1alpha1.AgentCredential
	}
	var todo []plan
	for _, cr := range ai.Spec.Credentials {
		k, err := credkindregistry.Get(cr.Type)
		if err != nil {
			// Unrecognized type: this walk is a survey over EVERY credential on
			// the identity looking for ones that NeedsRefresh, so one credential
			// of an unfamiliar type must not abort the whole command — but it
			// must not vanish with no trace either, so print a line marking it
			// skipped (never silently `continue`d, per the no-silent-errors rule).
			fmt.Fprintf(out, "-> %s: unrecognized credential type %q, skipped\n", cr.Name, cr.Type)
			continue
		}
		if !k.NeedsRefresh() {
			continue
		}
		if credName != "" && cr.Name != credName {
			continue
		}
		todo = append(todo, plan{cred: cr})
	}
	if credName != "" && len(todo) == 0 {
		return fmt.Errorf("AgentIdentity %q has no oauth credential named %q", identityName, credName)
	}

	var anyErr error
	for _, p := range todo {
		// Threshold check: skip when expires_at is more than `threshold` in the
		// future, unless --all-oauth or --credential is set.
		if !allOAuth && credName == "" {
			expiring, err := isExpiringSoon(ctx, c, namespace, p.cred, threshold)
			if err != nil {
				fmt.Fprintf(out, "x %s: %v\n", p.cred.Name, err)
				anyErr = err
				continue
			}
			if !expiring {
				fmt.Fprintf(out, "-> %s: not expiring within %s, skipped\n", p.cred.Name, threshold)
				continue
			}
		}
		if err := refresh.Run(ctx, c, namespace, p.cred); err != nil {
			fmt.Fprintf(out, "x %s: %v\n", p.cred.Name, err)
			anyErr = err
			continue
		}
		fmt.Fprintf(out, "ok %s refreshed\n", p.cred.Name)
	}
	if anyErr != nil {
		return fmt.Errorf("one or more credentials failed to refresh")
	}
	return nil
}

func isExpiringSoon(ctx context.Context, c client.Client, ns string,
	cred spiceboxv1alpha1.AgentCredential, threshold time.Duration) (bool, error) {
	if cred.OAuth == nil {
		return false, fmt.Errorf("credential %q has no oauth block", cred.Name)
	}
	var sec corev1.Secret
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: cred.OAuth.SecretRef.Name}, &sec); err != nil {
		return false, err
	}
	exp := string(sec.Data["expires_at"])
	if exp == "" {
		return false, nil // unknown expiry → not expiring
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return false, err
	}
	return time.Until(t) < threshold, nil
}
