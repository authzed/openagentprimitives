// Package fake is a deterministic federation.Minter for tests.
package fake

import (
	"context"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// Minter returns a token derived from the request, or Err when non-nil.
type Minter struct {
	// Err, when set, is returned from every Mint call.
	Err error
	// TTL is the lifetime stamped onto MintedToken.ExpiresAt (relative to
	// the Now func). Defaults to time.Hour when zero.
	TTL time.Duration
	// Now supplies the clock; defaults to time.Now.
	Now func() time.Time
	// Calls records every MintRequest for assertions.
	Calls []federation.MintRequest
}

func (m *Minter) Mint(_ context.Context, req federation.MintRequest) (federation.MintedToken, error) {
	m.Calls = append(m.Calls, req)
	if m.Err != nil {
		return federation.MintedToken{}, m.Err
	}
	ttl := m.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	return federation.MintedToken{
		AccessToken: sensitive.NewSensitiveValue([]byte("minted-for-" + req.Resource)),
		ExpiresAt:   now().Add(ttl),
	}, nil
}
