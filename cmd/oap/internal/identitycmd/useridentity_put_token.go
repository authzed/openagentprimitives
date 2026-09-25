package identitycmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/apcmd"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughcatalog"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/useridentity"
)

func newUserIdentityPutTokenCmd(g *apcmd.Globals) *cobra.Command {
	var subject, credName, token, displayName string
	var skipVerify bool
	cmd := &cobra.Command{
		Use:   "put-token",
		Short: "Seed a UserIdentity with a static token for one credential name",
		RunE: func(cmd *cobra.Command, args []string) error {
			if subject == "" || credName == "" || token == "" {
				return fmt.Errorf("--subject, --credential-name and --token are required")
			}
			b, err := g.Bundle()
			if err != nil {
				return err
			}
			return runUserIdentityPutToken(cmd.Context(), cmd.OutOrStdout(),
				b.Controller, subject, credName, token, displayName, skipVerify)
		},
	}
	cmd.Flags().StringVar(&subject, "subject", "", "Canonical SpiceDB subject, e.g. user:<base64(email)>")
	cmd.Flags().StringVar(&credName, "credential-name", "", "Credential name (must match what tools declare)")
	cmd.Flags().StringVar(&token, "token", "", "The token value")
	cmd.Flags().StringVar(&displayName, "display-name", "", "Human-friendly label for the UserIdentity")
	cmd.Flags().BoolVar(&skipVerify, "skip-verify", false,
		"Skip live verification of the token against the provider (format check still applies)")
	return cmd
}

func runUserIdentityPutToken(ctx context.Context, out io.Writer, c client.Client,
	subject, credName, token, displayName string, skipVerify bool) error {

	// Format gate (fail closed, no override), then live verification
	// (verifyGate policy). Both permissive when no provider resolves.
	var prov *provider.Provider
	if p, ok := passthroughcatalog.ProviderForCredential(ctx, c, credName); ok {
		prov = p
		if err := provider.ValidateToken(*p, token); err != nil {
			return fmt.Errorf("refusing to store credential %q: %w", credName, err)
		}
	}
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	verified, err := verifyGate(ctx, out, prov, builtins.StoreValue{Bearer: token}, skipVerify, os.Stdin, interactive)
	if err != nil {
		return fmt.Errorf("credential %q: %w", credName, err)
	}

	// The verification's own answer to "whose account is this?" is stored with
	// the credential. It is threaded from the check rather than re-derived,
	// so the recorded provider is always the one that actually ran the probe.
	if err := useridentity.PutToken(ctx, c, useridentity.PutTokenRequest{
		Subject:        identity.Subject(subject),
		CredentialName: credName,
		Token:          token,
		DisplayName:    displayName,
		ProviderID:     verified.ProviderID,
		SubjectID:      verified.SubjectID,
	}); err != nil {
		return err
	}
	name := useridentity.NameForSubject(identity.Subject(subject))
	fmt.Fprintf(out, "UserIdentity %s: credential %q stored\n", name, credName)
	return nil
}

// upsertCredential is a thin wrapper around the canonical implementation
// in pkg/platform/identity/useridentity, kept so existing tests in this package
// continue to exercise the slice-mutation contract from the CLI side.
func upsertCredential(creds []spiceboxv1alpha1.AgentCredential, c spiceboxv1alpha1.AgentCredential) []spiceboxv1alpha1.AgentCredential {
	return useridentity.UpsertCredential(creds, c)
}
