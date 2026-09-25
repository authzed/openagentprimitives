// Package passwordkind implements the "password" idp.Kind: a non-federated,
// password-only local IdP for the single-user macOS desktop's /admin console.
// Unlike oidckind/googlekind it never redirects to a remote authorization server
// — Begin points the browser at identityd's own same-origin /password/login form,
// and the credential check happens via the optional idp.PasswordVerifier
// capability (VerifyPassword), not Complete (which this kind never uses).
//
// The provider holds a bcrypt hash resolved from the ClusterIdentityProvider's
// ClientSecretRef Secret — idp.Config.ClientSecret is that opaque string here,
// never a real OAuth client secret — and a STABLE local Principal derived from
// idp.Config.ClientID, the local admin identifier. The Principal's
// Subject/Canonical must stay stable across restarts: it is the subject of the
// SpiceDB platform#admin relationship `oap platform grant-admin` writes.
package passwordkind

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/crypto/bcrypt"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/idp/registry"
)

// defaultLocalIdentity is the identifier the local admin Principal is derived
// from when the ClusterIdentityProvider's spec.clientID is empty. A fixed,
// well-known value ON PURPOSE: the desktop bundle has exactly one local admin
// account, so pinning the identifier keeps the Principal — and therefore its
// SpiceDB subject — stable across `oap idp setup password` re-runs that leave
// spec.clientID unset.
const defaultLocalIdentity = "admin@ap.local"

func init() { registry.Register(&Kind{}) }

// Kind is the "password" idp.Kind.
type Kind struct{}

func (k *Kind) Name() string { return "password" }

// New builds a Provider from cfg. cfg.ClientSecret is the resolved
// ClientSecretRef Secret value: a bcrypt password hash, not an OAuth client
// secret. It is validated eagerly — an empty or malformed hash is a
// misconfiguration that must fail loudly at construction, since otherwise every
// login either always fails or behaves unpredictably against
// bcrypt.CompareHashAndPassword on a garbage hash.
func (k *Kind) New(_ context.Context, cfg idp.Config) (idp.Provider, error) {
	if err := validateBcryptHash(cfg.ClientSecret); err != nil {
		return nil, fmt.Errorf("password idp: %w", err)
	}
	identifier := cfg.ClientID
	if identifier == "" {
		identifier = defaultLocalIdentity
	}
	// VerifiedEmail: a correct password IS this kind's proof of identity — there
	// is no separate email-verification step to mirror — and the Principal's
	// Subject/Canonical depends only on the lowercased identifier, so it is stable
	// across restarts and re-setups that leave spec.clientID alone.
	return &provider{
		hash:      cfg.ClientSecret,
		principal: identity.VerifiedEmail(identity.Email(identifier), "Local Admin"),
	}, nil
}

func (k *Kind) Wizard() idp.Wizard { return &wizard{} }

// ValidateSpec enforces that a password-kind IdP runs wide open
// (allowAnyEmail=true): the desktop bundle has one local admin account and no
// email-domain concept, so a spec setting AllowedEmailDomains instead is
// meaningless and must be rejected rather than silently accepted.
// webhooksettings.IdPSpecError already requires EITHER domains OR allowAnyEmail;
// this tightens it for kind=password.
func (k *Kind) ValidateSpec(spec spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	if !spec.AllowAnyEmail {
		return "spec.allowAnyEmail must be true for kind=password (a password kind has no email domains)"
	}
	return ""
}

// DiscoveryURL is "": the password kind is a local, non-federated IdP —
// identityd serves its login form itself — so once its Secret (the bcrypt hash)
// resolves it is ready, with no remote discovery probe.
func (k *Kind) DiscoveryURL(_ spiceboxv1alpha1.ClusterIdentityProviderSpec) string {
	return ""
}

// AllowedNonLocal is false: password is a single-user LOCAL admin gate with no
// external issuer to lean on for anti-brute-force protection. On a public cluster
// its /password/verify endpoint would be an unauthenticated brute-force (and
// bcrypt-cost-tunable DoS) surface exposed straight to the internet, so the kind
// is local-only by construction — enforced structurally through this method by
// the ClusterIdentityProvider validity controller, not by documentation.
func (k *Kind) AllowedNonLocal() bool { return false }

// validateBcryptHash rejects anything that cannot be a valid bcrypt hash.
// Intentionally strict (non-empty, "$2" prefix, parses via bcrypt.Cost) so a
// truncated Secret, a plaintext password stored unhashed, or an empty
// ClientSecretRef key all fail closed at Kind.New rather than silently making
// every VerifyPassword call behave unpredictably.
func validateBcryptHash(hash string) error {
	if hash == "" {
		return errors.New("client secret (bcrypt password hash) is empty")
	}
	if !strings.HasPrefix(hash, "$2") {
		return errors.New(`client secret does not look like a bcrypt hash (must start with "$2")`)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		return fmt.Errorf("client secret is not a well-formed bcrypt hash: %w", err)
	}
	return nil
}

// provider implements idp.Provider (+ idp.PasswordVerifier) for the
// password kind.
type provider struct {
	hash      string
	principal identity.Principal
}

// Begin returns a same-origin path — identityd's own password-entry form —
// instead of a remote IdP authorize URL. state is round-tripped as a query
// parameter: /password/login validates it (without consuming) before rendering
// the form, and /password/verify consumes it on submit. A redirect-based Kind's
// Begin embeds state in the authorize URL the same way; only the destination
// differs.
func (p *provider) Begin(_ context.Context, state string) (string, error) {
	return "/password/login?state=" + url.QueryEscape(state), nil
}

// Complete is never invoked for the password kind: identityd dispatches this
// kind's login straight to /password/verify via idp.PasswordVerifier, never
// through the OIDC callback endpoint that calls Complete. Implemented only to
// satisfy idp.Provider, and errors clearly if ever reached — e.g. a caller
// mistakenly wiring the generic OIDC callback to a password-kind IdP.
func (p *provider) Complete(_ context.Context, _ idp.CallbackParams) (identity.Principal, *idp.TokenSet, error) {
	return identity.Principal{}, nil, errors.New("password idp: authenticates via /password/verify, not the OIDC callback")
}

// VerifyPassword implements idp.PasswordVerifier. A bcrypt comparison failure
// (wrong password OR a hash/cost mismatch) and a match both take constant-ish
// time via bcrypt's own comparison, so no extra timing countermeasure is added
// here; identityd's caller adds a fixed delay on the failure path to blunt online
// brute-force attempts.
func (p *provider) VerifyPassword(password string) (identity.Principal, bool) {
	if bcrypt.CompareHashAndPassword([]byte(p.hash), []byte(password)) != nil {
		return identity.Principal{}, false
	}
	return p.principal, true
}
