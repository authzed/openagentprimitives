// Package mcpfront is the externally-reachable MCP surface: the OAuth
// minter, the /mcp bearer middleware, and the typed ops layer.
package mcpfront

import (
	"context"
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/spicedb"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
)

// GrantWriter is the SpiceDB half of minting: write the token's complete
// tuple set, or tear it back down on a compensating failure. Satisfied by
// *spicedb.Client; declared here (not imported from spicedb) so Minter's
// dependency is exactly the two calls it makes, and a fake is trivial to
// write in tests.
type GrantWriter interface {
	WriteAccessTokenGrant(ctx context.Context, g spicedb.AccessTokenGrant) error
	DeleteAccessTokenTuples(ctx context.Context, tokenID string) error
}

// Minter mints an AccessToken: a SpiceDB grant (the authorization truth) plus
// an AccessToken CR (the authentication record — hash, owner, display
// metadata, expiry). See MintAccessToken's doc for why the two are written in
// that order and what happens when the second half fails.
type Minter struct {
	SpiceDB GrantWriter
	K8s     client.Client
	// Namespace is where the AccessToken CR is created (webd flag
	// --accesstoken-namespace, default "agentprimitives-system").
	Namespace string
	// Lifetime is how long a minted token lives (webd flag
	// --accesstoken-lifetime, default 90*24h).
	Lifetime time.Duration
	// Clock stands in for time.Now in tests; nil uses the real clock (see now()).
	Clock func() time.Time
}

// MintParams is what the OAuth token endpoint has decided to grant: the
// subject who approved it, the role/scope ladder they chose, and the client
// display fields the AccessToken CR surfaces back to an admin.
type MintParams struct {
	Owner        identity.CanonicalUserID
	Role         string
	ScopeClasses []string
	Unfiltered   bool
	ClientName   string
	ClientID     string
}

// Minted is the result of a successful mint. Value is the ONE AND ONLY time
// the plaintext bearer value is ever surfaced — it is never stored (the CR
// carries only its hash) and never retrievable again.
type Minted struct {
	Value     string
	TokenID   string
	ExpiresAt time.Time
}

// now returns the current time, substituting Clock in tests.
func (m *Minter) now() time.Time {
	if m.Clock != nil {
		return m.Clock()
	}
	return time.Now()
}

// MintAccessToken mints a fresh token for p: a random value + id, a SpiceDB
// grant carrying the role/scope, and an AccessToken CR carrying the value's
// hash plus display metadata.
//
// Tuples are written FIRST, then the CR is created. This ordering matters:
// the /mcp bearer middleware authenticates a request by looking up the
// AccessToken CR's hash and then checking SpiceDB (fully consistent) for the
// matching grant. Writing SpiceDB first means the token is fully usable the
// instant this call returns — there is no reconciler race where a freshly
// minted token's first use could find tuples not yet written. Writing the CR
// first would invert that: a client could present a token that authenticates
// (the CR exists) before it is actually authorized, or worse, a crash between
// the two writes would leave a CR with no backing grant that could later be
// mistaken for a revoked-but-still-recognized token.
//
// If the CR create fails after the grant is written, this compensates by
// deleting the SpiceDB tuples it just wrote (best effort) and returns both
// errors joined — a grant with no corresponding CR is unreachable (nothing
// can ever present the matching bearer value) but would otherwise clutter
// SpiceDB forever.
func (m *Minter) MintAccessToken(ctx context.Context, p MintParams) (Minted, error) {
	var out Minted
	if p.Owner.IsZero() {
		return out, fmt.Errorf("mint access token: empty owner")
	}
	if _, err := accesstoken.RoleRelation(p.Role); err != nil {
		return out, fmt.Errorf("mint access token: %w", err)
	}
	if !p.Unfiltered && len(p.ScopeClasses) == 0 {
		return out, fmt.Errorf("mint access token: scope classes or unfiltered required")
	}

	value, err := accesstoken.NewTokenValue()
	if err != nil {
		return out, fmt.Errorf("mint access token: %w", err)
	}
	tokenID, err := accesstoken.NewTokenID()
	if err != nil {
		return out, fmt.Errorf("mint access token: %w", err)
	}

	now := m.now()
	expires := now.Add(m.Lifetime)

	// Tuples FIRST: the first /mcp check is fully consistent, so a token is
	// usable the instant the endpoint responds — no reconciler race.
	if err := m.SpiceDB.WriteAccessTokenGrant(ctx, spicedb.AccessTokenGrant{
		TokenID:      tokenID,
		Owner:        p.Owner,
		Role:         p.Role,
		ScopeClasses: p.ScopeClasses,
		Unfiltered:   p.Unfiltered,
		ExpiresAt:    expires,
	}); err != nil {
		return out, fmt.Errorf("mint access token %s: write grant: %w", tokenID, err)
	}

	cr := &spiceboxv1alpha1.AccessToken{
		ObjectMeta: metav1.ObjectMeta{Name: tokenID, Namespace: m.Namespace},
		Spec: spiceboxv1alpha1.AccessTokenSpec{
			TokenHash:  accesstoken.HashTokenValue(value),
			Owner:      p.Owner.String(),
			ClientName: p.ClientName,
			ClientID:   p.ClientID,
			ExpiresAt:  metav1.NewTime(expires),
		},
	}
	if err := m.K8s.Create(ctx, cr); err != nil {
		// Compensate: a grant without its authentication record is
		// unreachable but would clutter SpiceDB; best-effort removal, and
		// the failure of THAT is surfaced by joining it into the returned
		// error rather than being swallowed.
		delErr := m.SpiceDB.DeleteAccessTokenTuples(ctx, tokenID)
		return out, errors.Join(
			fmt.Errorf("mint access token %s: create AccessToken CR: %w", tokenID, err),
			delErr,
		)
	}

	return Minted{Value: value, TokenID: tokenID, ExpiresAt: expires}, nil
}
