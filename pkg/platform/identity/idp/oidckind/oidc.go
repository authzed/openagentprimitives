// Package oidckind implements the generic OIDC idp kind — issuer
// discovery + code exchange + ID-token verification via go-oidc.
// The google kind is a preset over this package via Options.
package oidckind

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

func init() { registry.Register(&Kind{}) }

// Options customize the generic flow for preset kinds (google).
type Options struct {
	// ExtraAuthParams are appended to the authorize URL (e.g. hd=…).
	ExtraAuthParams map[string]string
	// VerifyClaims, when non-nil, runs against the verified ID token's
	// raw claims after standard verification — preset kinds re-check
	// provider-specific claims (google's hd) here.
	VerifyClaims func(claims map[string]any) error
}

// Kind is the OIDC idp.Kind, registered under the name "oidc".
type Kind struct{}

func (k *Kind) Name() string { return "oidc" }

func (k *Kind) New(ctx context.Context, cfg idp.Config) (idp.Provider, error) {
	return NewWithOptions(ctx, cfg, Options{})
}

func (k *Kind) Wizard() idp.Wizard { return &wizard{} }

// ValidateSpec re-affirms the non-empty-issuer requirement that
// webhooksettings.IdPSpecError already enforces. There is no further
// oidc-specific rule.
func (k *Kind) ValidateSpec(spec spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	if spec.Issuer == "" {
		return "spec.issuer is required for kind=oidc"
	}
	return ""
}

// DiscoveryURL is the generic OIDC discovery endpoint derived from
// spec.Issuer.
func (k *Kind) DiscoveryURL(spec spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return spec.Issuer + "/.well-known/openid-configuration"
}

// AllowedNonLocal is true: oidc authenticates against a remote, operator-
// chosen OIDC issuer, so it carries no local-only brute-force exposure.
func (k *Kind) AllowedNonLocal() bool { return true }

// NewWithOptions is the constructor preset kinds (google) reuse.
func NewWithOptions(ctx context.Context, cfg idp.Config, opts Options) (idp.Provider, error) {
	// Every OIDC outbound call — issuer discovery, JWKS key fetch, token exchange
	// — targets a URL derived from operator-supplied (and partly
	// remote-document-derived) config, so route them through the SSRF-guarded
	// client: a hostile or typo'd issuer must not pivot the operator at the
	// cloud-metadata endpoint or an in-cluster address. Tests inject their own
	// loopback client via ClientContext, which the guard would block, so fall back
	// to safehttp only when the caller supplied none.
	hc := clientFromContext(ctx)
	if hc == nil {
		hc = safehttp.Client()
		ctx = gooidc.ClientContext(ctx, hc)
	}

	op, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", cfg.Issuer, err)
	}
	scopes := append([]string{gooidc.ScopeOpenID, "email", "profile"}, cfg.Scopes...)
	// When federation is enabled, request offline_access so the IdP returns
	// a refresh token that identityd can capture for ID-JAG minting.
	if cfg.Federation {
		scopes = append(scopes, "offline_access")
	}
	return &provider{
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     op.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       scopes,
		},
		// op carries hc (set via the ctx above), so the verifier's lazy
		// RemoteKeySet JWKS fetch inherits the guarded client too.
		verifier:      op.Verifier(&gooidc.Config{ClientID: cfg.ClientID}),
		httpClient:    hc,
		opts:          opts,
		cfg:           cfg,
		tokenEndpoint: op.Endpoint().TokenURL,
	}, nil
}

// clientFromContext returns the *http.Client a caller injected via
// gooidc.ClientContext / oauth2's context key, or nil. ClientContext stores under
// oauth2.HTTPClient, so reading that key recovers it without a go-oidc accessor.
func clientFromContext(ctx context.Context) *http.Client {
	c, _ := ctx.Value(oauth2.HTTPClient).(*http.Client)
	return c
}

type provider struct {
	oauth         oauth2.Config
	verifier      *gooidc.IDTokenVerifier
	httpClient    *http.Client
	opts          Options
	cfg           idp.Config
	tokenEndpoint string
}

// Begin returns the IdP authorize URL. nonce is set to state so the ID
// token is bound to the same single-use random material identityd
// uses for CSRF protection — replay protection without a second store.
func (p *provider) Begin(_ context.Context, state string) (string, error) {
	extra := []oauth2.AuthCodeOption{gooidc.Nonce(state)}
	for k, v := range p.opts.ExtraAuthParams {
		extra = append(extra, oauth2.SetAuthURLParam(k, v))
	}
	return p.oauth.AuthCodeURL(state, extra...), nil
}

// Complete exchanges the code for tokens and verifies the ID token — signature
// (via go-oidc JWKS), issuer, audience, expiry, and nonce — returning the
// verified Principal plus, when federation is on and the IdP returned a refresh
// token, a non-nil TokenSet. EmailVerified mirrors the provider claim; policy is
// identityd's responsibility.
func (p *provider) Complete(ctx context.Context, cb idp.CallbackParams) (identity.Principal, *idp.TokenSet, error) {
	if cb.Error != "" {
		return identity.Principal{}, nil, fmt.Errorf("idp returned error: %s", cb.Error)
	}
	// Bind the guarded client onto this call's context so the oauth2 token
	// exchange (and any verify-time fetch) uses it, not http.DefaultClient.
	if p.httpClient != nil {
		ctx = gooidc.ClientContext(ctx, p.httpClient)
	}
	tok, err := p.oauth.Exchange(ctx, cb.Code)
	if err != nil {
		return identity.Principal{}, nil, fmt.Errorf("code exchange: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return identity.Principal{}, nil, fmt.Errorf("token response missing id_token")
	}
	idTok, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return identity.Principal{}, nil, fmt.Errorf("id_token verify: %w", err)
	}
	if idTok.Nonce != cb.State {
		return identity.Principal{}, nil, fmt.Errorf("id_token nonce mismatch")
	}
	var claims struct {
		// The identity the canonical subject is derived from; empty is refused
		// below, since a login with no email names nobody.
		Email string `json:"email"`
		// The IdP's own assertion that it proved Email. Mirrored onto the
		// Principal verbatim — whether an unverified email may log in is
		// identityd's policy, not this kind's.
		EmailVerified boolOrString `json:"email_verified"`
		// Display name only; never used for identity or authorization.
		Name string `json:"name"`
	}
	if err := idTok.Claims(&claims); err != nil {
		return identity.Principal{}, nil, fmt.Errorf("id_token claims: %w", err)
	}
	if p.opts.VerifyClaims != nil {
		var raw map[string]any
		if err := idTok.Claims(&raw); err != nil {
			return identity.Principal{}, nil, fmt.Errorf("id_token raw claims: %w", err)
		}
		if err := p.opts.VerifyClaims(raw); err != nil {
			return identity.Principal{}, nil, fmt.Errorf("provider claim check: %w", err)
		}
	}
	if claims.Email == "" {
		return identity.Principal{}, nil, fmt.Errorf("id_token has no email claim")
	}
	principal := identity.IdPUser(identity.Email(claims.Email), bool(claims.EmailVerified), claims.Name)

	// Populated only when federation was requested AND the IdP returned a refresh
	// token; nil otherwise.
	var ts *idp.TokenSet
	if p.cfg.Federation {
		rt := tok.RefreshToken
		if rt != "" {
			expiresAt := tok.Expiry
			if expiresAt.IsZero() {
				expiresAt = time.Now().Add(time.Hour)
			}
			ts = &idp.TokenSet{
				RefreshToken:  rt,
				AccessToken:   tok.AccessToken,
				TokenEndpoint: p.tokenEndpoint,
				ClientID:      p.cfg.ClientID,
				ClientSecret:  p.cfg.ClientSecret,
				Scope:         strings.Join(p.oauth.Scopes, " "),
				ExpiresAt:     expiresAt,
			}
		}
	}

	return principal, ts, nil
}

// boolOrString tolerates providers (Google, historically) that emit
// email_verified as the string "true"/"false" instead of a JSON bool.
type boolOrString bool

func (b *boolOrString) UnmarshalJSON(data []byte) error {
	switch string(data) {
	case "true", `"true"`:
		*b = true
	case "false", `"false"`, "null":
		*b = false
	default:
		return fmt.Errorf("email_verified: unexpected value %s", data)
	}
	return nil
}
