// Package idp defines the pluggable human-login identity-provider
// seam: cluster config (ClusterIdentityProvider) names a Kind; the
// Kind constructs a Provider; identityd drives Begin/Complete and
// enforces email-verified + allowed-domain policy on the result.
package idp

import (
	"context"
	"errors"
	"time"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/cli/tui"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// Config is the kind-agnostic provider configuration resolved from
// the ClusterIdentityProvider CR + its client-secret Secret.
type Config struct {
	Issuer          string
	ClientID        string
	ClientSecret    string
	Scopes          []string // extras beyond openid/email/profile
	RedirectURL     string   // "<external>/oidc/callback/idp"
	LoginHintDomain string   // first allowed domain; UX hint ONLY, never trusted
	// Federation, when true, requests offline_access so the user's refresh
	// token is captured at login and written into a subject-keyed Secret for
	// ID-JAG federation.
	Federation bool
}

// TokenSet carries the OAuth token material captured at login for
// federation (offline_access). Nil when the IdP returned no refresh token
// (e.g. kind=google, or federation disabled). All fields are plaintext at
// this boundary; identityd writes them straight into a Secret.
type TokenSet struct {
	RefreshToken  string
	AccessToken   string
	TokenEndpoint string
	ClientID      string
	ClientSecret  string
	Scope         string
	ExpiresAt     time.Time
}

// CallbackParams carries the IdP callback's query inputs.
type CallbackParams struct {
	Code  string
	State string
	Error string // IdP-reported error (user aborted, etc.)
}

// Provider is one configured identity provider instance.
type Provider interface {
	// Begin returns the IdP authorize URL. state is opaque CSRF
	// material identityd owns and verifies at the callback.
	Begin(ctx context.Context, state string) (redirectURL string, err error)
	// Complete exchanges the callback for a verified Principal and, when the
	// flow captured refresh material (offline_access), a non-nil TokenSet.
	// It MUST verify the ID token (signature, issuer, audience, nonce) —
	// never trust the userinfo endpoint alone. EmailVerified mirrors the
	// provider's claim; policy enforcement is identityd's job.
	Complete(ctx context.Context, cb CallbackParams) (identity.Principal, *TokenSet, error)
}

// Kind constructs Providers; registered in init() via registry.Register.
//
// ValidateSpec and DiscoveryURL exist so the ClusterIdentityProvider validity
// controller can drive its generic-webhook-then-kind-specific gates entirely
// through the registry, with NO per-kind branching in the controller. A new
// kind's spec-validation and readiness-probe behavior is added here, once, and
// the controller picks it up via registry.Get.
type Kind interface {
	Name() string
	New(ctx context.Context, cfg Config) (Provider, error)
	Wizard() Wizard

	// ValidateSpec performs kind-specific spec validation BEYOND the generic
	// webhook check (webhooksettings.IdPSpecError, which every kind shares). It
	// returns a human-readable denial when spec is invalid FOR THIS KIND (e.g.
	// password requires AllowAnyEmail), or "" when acceptable. Called after the
	// generic webhook gate passes and before the Secret-resolvability gate.
	ValidateSpec(spec spiceboxv1alpha1.ClusterIdentityProviderSpec) string

	// DiscoveryURL returns the OIDC discovery endpoint the validity controller
	// GETs as its readiness probe, derived from spec (a pinned issuer for google,
	// spec.Issuer for generic oidc). "" means the kind needs NO remote discovery
	// probe at all, and is ready as soon as its Secret resolves (gate 2).
	DiscoveryURL(spec spiceboxv1alpha1.ClusterIdentityProviderSpec) string

	// AllowedNonLocal reports whether this kind may be configured on a non-local
	// (real/public) cluster. Kinds backed by a remote OIDC issuer (oidc, google)
	// authenticate against a third party and return true. A local-only kind — no
	// external issuer, weak anti-brute-force posture, such as password — returns
	// false, and the ClusterIdentityProvider validity controller refuses it with
	// Valid=False/ConfigInvalid on any cluster that is not local.
	//
	// "Local" comes from the cluster kind `oap install` stamped
	// (cloud.Strategy.InstallProfile().AllowsLocalOnlyIdentityProviders()), NOT
	// from anything that merely correlates with it such as the memory backend:
	// Postgres runs locally and sqlite runs remotely, and conflating the two
	// silently flips this gate. A structural guard, not documentation — a new
	// local-only kind is fenced off the moment it returns false, with no per-kind
	// branch anywhere else.
	AllowedNonLocal() bool
}

// PasswordVerifier is an OPTIONAL capability a Provider implements when its kind
// authenticates by password rather than a redirect-based federated flow.
// identityd type-asserts a resolved Provider to it at the password-verify
// endpoint and FAILS CLOSED when the assertion misses.
//
// VerifyPassword returns the Provider's stable Principal + true on a match, the
// zero Principal + false otherwise. It never errors: "wrong password" and "no
// password configured" are both just "not authenticated" to the caller.
// Configuration errors are the constructor's (Kind.New) to report.
type PasswordVerifier interface {
	VerifyPassword(password string) (identity.Principal, bool)
}

// Wizard is the interactive `oap idp setup <kind>` flow.
//
// Same two-call shape as channelkinds.Wizard, for the same reason: a wizard
// DESCRIBES its questions and reads its answers back out of the State, while
// `cmd/oap` owns how they are presented — terminal detection, theme, driver
// selection, chrome and the post-run summary. One answer to "how does an `oap`
// wizard look" instead of one per contract.
//
// Unlike builtins.Flow, Result takes neither a ctx nor its input: a WizardOutput
// is derived from the answered State alone, and the dispatcher does every write.
type Wizard interface {
	// Screens describes the wizard as a sequence. A nil error MUST come with at
	// least one screen: a wizard that asks nothing would go on to apply whatever
	// empty spec Result derived from an unanswered State.
	Screens(ctx context.Context, in WizardInput) ([]tui.Screen, error)

	// Result reads the answered State and produces the CR spec plus the Secret
	// material to write. Called only after a successful run of the screens
	// Screens returned, on the same Wizard value — an implementation may carry
	// its WizardInput across the two calls.
	//
	// It is the fail-closed choke point. huh's accessible renderer has no error
	// channel, so a run whose input ran out arrives here as an unanswered State
	// and a nil error; a Result that accepted one would configure cluster login
	// against an empty issuer with an empty credential, behind a CLI that
	// reported success.
	Result(st *tui.State) (WizardOutput, error)
}

// unavailableWizard is the Wizard for a kind that has no setup flow.
type unavailableWizard struct{ reason string }

// UnavailableWizard returns a Wizard that refuses with reason.
//
// A kind whose ClusterIdentityProvider is built by something other than
// `oap idp setup` — the in-process test kind — still needs a non-nil Wizard, so an
// accidental `oap idp setup <kind>` says why it cannot run rather than
// dereferencing nil or running a zero-screen wizard.
func UnavailableWizard(reason string) Wizard { return unavailableWizard{reason: reason} }

func (w unavailableWizard) Screens(context.Context, WizardInput) ([]tui.Screen, error) {
	return nil, errors.New(w.reason)
}

func (w unavailableWizard) Result(*tui.State) (WizardOutput, error) {
	return WizardOutput{}, errors.New(w.reason)
}

// WizardInput is what the CLI hands the wizard.
type WizardInput struct {
	// CallbackURL is the derived redirect URI ("<external>/oidc/callback/idp")
	// the wizard MUST show for console registration before asking for
	// credentials.
	CallbackURL string
	// Existing is the current ClusterIdentityProvider spec when re-running setup
	// for the SAME kind, or nil on first setup / kind mismatch. Wizards pre-fill
	// non-secret fields (issuer, client ID, allowed domains) from it. The client
	// secret is never carried here — it lives in a Secret and is never echoed.
	Existing *spiceboxv1alpha1.ClusterIdentityProviderSpec
	// SecretExists reports whether the referenced client-secret Secret already
	// exists, so the wizard can offer keep-on-blank instead of requiring re-entry.
	SecretExists bool
}

// WizardOutput is what the CLI dispatcher applies to the cluster.
// SecretData == nil signals "keep the existing Secret untouched" (re-setup with
// an unchanged secret); the dispatcher skips writing the Secret in that case.
type WizardOutput struct {
	Spec       spiceboxv1alpha1.ClusterIdentityProviderSpec
	SecretName string            // Secret to create/update in the operator namespace
	SecretData map[string][]byte // client secret material; nil ⇒ keep existing
}
