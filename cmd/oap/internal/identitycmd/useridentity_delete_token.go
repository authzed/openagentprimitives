package identitycmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func newUserIdentityDeleteTokenCmd(g *apcmd.Globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete-token <name> <credential>",
		Short: "Remove one credential from a UserIdentity and delete its backing Secret",
		Long: `Remove the named credential from a UserIdentity's spec.credentials and
delete that credential's backing Secret in the identities namespace, leaving
the identity's other credentials (and their Secrets) intact.

Use this to unlink a credential that was linked to the wrong account — for
example an "anthropic-oauth" token captured against the wrong workspace — so
it can be re-linked. The next agent session that needs this credential will
re-prompt the user to re-link it via the "Connect your accounts" flow.

<name> is the UserIdentity metadata.name (the "u-…" hash, as shown by
"oap user-identity list"); <credential> is the credential name to remove.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runUserIdentityDeleteToken(cmd.Context(), cmd.OutOrStdout(),
				b.Controller, args[0], args[1])
		},
	}
	return cmd
}

// runUserIdentityDeleteToken removes credName from the UserIdentity named
// uiName and deletes that credential's master Secret in the identities
// namespace, leaving all other credentials untouched.
//
// The spec edit is a typed get-modify-update: the client's Update carries the
// read resourceVersion, so a concurrent spec change conflicts (and surfaces as
// an error) rather than silently clobbering the wrong entry. The spec is
// updated before the Secret is deleted so a mid-operation failure leaves an
// orphan Secret (harmless, GC-able) rather than a credential pointing at a
// now-missing Secret. It refuses with a non-zero exit when the credential is
// not present, rather than silently no-oping.
func runUserIdentityDeleteToken(ctx context.Context, out io.Writer, c client.Client,
	uiName, credName string) error {

	var ui spiceboxv1alpha1.UserIdentity
	if err := c.Get(ctx, client.ObjectKey{Name: uiName}, &ui); err != nil {
		return fmt.Errorf("get UserIdentity %s: %w", uiName, err)
	}

	remaining, found := removeCredential(ui.Spec.Credentials, credName)
	if !found {
		return fmt.Errorf("UserIdentity %s has no credential named %q", uiName, credName)
	}
	ui.Spec.Credentials = remaining
	if err := c.Update(ctx, &ui); err != nil {
		return fmt.Errorf("update UserIdentity %s: %w", uiName, err)
	}
	fmt.Fprintf(out, "UserIdentity %s: removed credential %q\n", uiName, credName)

	secretName := useridentity.MasterSecretName(uiName, credName)
	secObj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: spiceboxv1alpha1.IdentitiesNamespace,
		},
	}
	switch err := c.Delete(ctx, secObj); {
	case apierrors.IsNotFound(err):
		fmt.Fprintf(out, "  secret %s/%s already absent\n", spiceboxv1alpha1.IdentitiesNamespace, secretName)
	case err != nil:
		return fmt.Errorf("delete secret %s/%s: %w", spiceboxv1alpha1.IdentitiesNamespace, secretName, err)
	default:
		fmt.Fprintf(out, "  deleted secret %s/%s\n", spiceboxv1alpha1.IdentitiesNamespace, secretName)
	}

	fmt.Fprintf(out, "The next session that needs %q will re-prompt to re-link it via \"Connect your accounts\".\n", credName)
	return nil
}

// removeCredential returns creds without the entry whose Name == name, plus
// whether such an entry was present. The result is a freshly allocated slice
// so the caller never mutates the original backing array.
func removeCredential(creds []spiceboxv1alpha1.AgentCredential, name string) ([]spiceboxv1alpha1.AgentCredential, bool) {
	out := make([]spiceboxv1alpha1.AgentCredential, 0, len(creds))
	found := false
	for _, cred := range creds {
		if cred.Name == name {
			found = true
			continue
		}
		out = append(out, cred)
	}
	return out, found
}
