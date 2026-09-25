// Package googlekind is the "google" idp kind: the generic oidc kind
// preset with Google's pinned issuer, an hd= login hint, and a
// server-side re-check of the hd claim (the hint is UX; the claim
// check is the enforcement).
package googlekind

import (
	"context"
	"fmt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/oidckind"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// Issuer is Google's pinned OIDC issuer.
const Issuer = "https://accounts.google.com"

func init() { registry.Register(&Kind{}) }

// Kind is the "google" idp.Kind.
type Kind struct{}

func (k *Kind) Name() string { return "google" }

func (k *Kind) New(ctx context.Context, cfg idp.Config) (idp.Provider, error) {
	if cfg.Issuer != "" {
		return nil, fmt.Errorf("google kind has a pinned issuer (%q); spec.issuer must be empty (got %q)", Issuer, cfg.Issuer)
	}
	cfg.Issuer = Issuer
	return oidckind.NewWithOptions(ctx, cfg, GoogleOptions(cfg.LoginHintDomain))
}

func (k *Kind) Wizard() idp.Wizard { return &wizard{} }

// ValidateSpec has no google-specific rule: webhooksettings.IdPSpecError already
// rejects a non-empty spec.Issuer for kind=google.
func (k *Kind) ValidateSpec(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}

// DiscoveryURL is pinned to Google's issuer regardless of spec — the
// google-issuer default lives HERE, not in the validity controller.
func (k *Kind) DiscoveryURL(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return Issuer + "/.well-known/openid-configuration"
}

// AllowedNonLocal is true: google authenticates against Google's remote
// OIDC issuer, so it carries no local-only brute-force exposure.
func (k *Kind) AllowedNonLocal() bool { return true }

// GoogleOptions builds the preset Options for the google kind: hd login hint plus
// server-side hd claim verification. hint=="" yields zero Options, so no hd
// handling. Exported so tests can build providers via oidckind.NewWithOptions
// against a fake issuer rather than Google's own.
func GoogleOptions(hint string) oidckind.Options {
	if hint == "" {
		return oidckind.Options{}
	}
	return oidckind.Options{
		// hd here is a UX hint only; VerifyClaims is the real enforcement.
		ExtraAuthParams: map[string]string{"hd": hint},
		VerifyClaims: func(claims map[string]any) error {
			hd, _ := claims["hd"].(string)
			if hd != hint {
				return fmt.Errorf("hd claim %q does not match required hosted domain %q", hd, hint)
			}
			return nil
		},
	}
}
