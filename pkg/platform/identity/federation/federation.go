// Package federation derives an upstream resource access token from a
// user's enterprise identity via the ID-JAG two-legged exchange
// (RFC 8693 token-exchange at the IdP → JWT authorization grant at the
// resource AS). It is the EMA-client seam: DI'd, exactly one concrete is
// chosen by the binary at startup (like llm.Provider), with a fake for
// tests. A nil Minter reaching a federated credential is a fail-closed
// configuration error, never a panic.
package federation

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// Minter mints an upstream access token from a user's enterprise identity.
type Minter interface {
	// Mint performs the ID-JAG exchange for req and returns the resulting
	// upstream access token plus its expiry. Implementations MUST NOT log
	// token bytes and MUST surface which leg failed in returned errors.
	Mint(ctx context.Context, req MintRequest) (MintedToken, error)
}

// SubjectMaterial is the user's enterprise identity, freshly resolved.
type SubjectMaterial struct {
	// Token is the user's current IdP token, presented as the
	// token-exchange subject_token. Wrapped so it can't be logged.
	Token sensitive.SensitiveValue
	// IdPTokenEndpoint is the IdP's RFC 8693 token endpoint (leg 1 target).
	IdPTokenEndpoint string
	// ClientID / ClientSecret identify ap as the registered client app at
	// the IdP (confidential client; both required for leg 1).
	ClientID     string
	ClientSecret string
}

// MintRequest names the resource the upstream token is for.
type MintRequest struct {
	Subject SubjectMaterial
	// Resource is the identifier the IdP knows the MCP server by — the
	// ID-JAG audience requested in leg 1.
	Resource string
	// ResourceServerURL is the MCP server URL whose authorization server
	// (discovered via RFC 9728 → 8414) accepts the ID-JAG in leg 2.
	ResourceServerURL string
	// Scopes are optional scopes requested at the resource AS in leg 2.
	Scopes []string
}

// MintedToken is the upstream access token plus its expiry. The token is
// memory-only; callers MUST NOT persist it to a Secret.
type MintedToken struct {
	AccessToken sensitive.SensitiveValue
	ExpiresAt   time.Time
}
