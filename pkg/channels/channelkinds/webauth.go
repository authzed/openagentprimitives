package channelkinds

import (
	"context"
	"errors"
)

// WebAuthenticator is the OIDC seam identityd uses to prove a visitor is
// the same canonical user the agent will act as. Each channel kind that
// can authenticate humans provides one; kinds that can't return nil from
// Kind.WebAuthenticator (those kinds simply aren't usable for passthrough
// self-service).
type WebAuthenticator interface {
	// Begin returns the URL the user should be redirected to in order to
	// start the OAuth flow. `state` is an opaque value the caller wants
	// echoed back via Complete — typically a CSRF/session token.
	Begin(ctx context.Context, state string) (redirectURL string, err error)

	// Complete consumes the OAuth callback parameters (the kind decides
	// which it needs — typically `code` + `state`) and returns the
	// canonical SpiceDB subject ("user:<base64(email)>") of the
	// authenticated user. The form values come from the OAuth callback's
	// query string.
	Complete(ctx context.Context, callback CallbackParams) (canonical string, err error)
}

// CallbackParams is the kind-agnostic shape of the OAuth callback's
// inputs. The Slack impl reads `code`, `state`, and may emit an `error`
// for the user-aborted-the-flow case.
type CallbackParams struct {
	Code  string
	State string
	Error string
}

// WebAuthDeps is the kind-agnostic dependency shape for constructing a
// WebAuthenticator. The slack kind reads ClientID + ClientSecret + the
// external base URL (for computing the redirect URL). identityd loads
// these from Kubernetes Secrets/ConfigMaps and hands them to each kind.
type WebAuthDeps struct {
	// ClientID is the OAuth app client ID for this channel kind.
	ClientID string
	// ClientSecret is the OAuth app client secret.
	ClientSecret string
	// ExternalBaseURL is identityd's externally reachable URL (e.g.
	// "https://identityd.example.com"). The redirect URL is
	// "<ExternalBaseURL>/oidc/callback/<kind-name>".
	ExternalBaseURL string
	// InstalledTeamID is the kind-specific tenant identifier the bot is
	// installed into (a Slack team_id). Authenticators MUST use it to
	// constrain OIDC sign-in to that tenant, so a user from a different
	// workspace whose profile email matches a victim's cannot impersonate them
	// through the canonical-email collision in Principal.Canonical(). Kinds
	// with no tenant boundary may ignore it.
	InstalledTeamID string
}

// ErrAuthenticatorUnavailable is the sentinel error WebAuthenticator
// methods return when the underlying configuration is missing (e.g. the
// Slack OAuth Secret is empty). identityd surfaces this as a "you need
// to configure $thing" error rather than crashing.
var ErrAuthenticatorUnavailable = errors.New("webauth: authenticator unavailable")
