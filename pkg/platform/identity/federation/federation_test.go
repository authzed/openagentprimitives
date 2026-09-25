package federation_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation/fake"
)

func TestFakeMinter_DerivesTokenAndStampsExpiry(t *testing.T) {
	fixed := time.Date(2026, 6, 19, 0, 0, 0, 0, time.UTC)
	m := &fake.Minter{TTL: 30 * time.Minute, Now: func() time.Time { return fixed }}

	got, err := m.Mint(context.Background(), federation.MintRequest{Resource: "linear"})
	require.NoError(t, err)
	assert.Equal(t, "minted-for-linear", string(got.AccessToken.UnderlyingValue()))
	assert.Equal(t, fixed.Add(30*time.Minute), got.ExpiresAt)
	require.Len(t, m.Calls, 1)
	assert.Equal(t, "linear", m.Calls[0].Resource)
}

func TestFakeMinter_ReturnsConfiguredError(t *testing.T) {
	m := &fake.Minter{Err: assert.AnError}
	_, err := m.Mint(context.Background(), federation.MintRequest{})
	require.ErrorIs(t, err, assert.AnError)
}
