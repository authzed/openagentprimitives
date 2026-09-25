package identityd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

func TestOAuthStateStore_RoundTrip(t *testing.T) {
	s := newOAuthStateStore(10 * time.Minute)
	tok, err := s.NewState(oauthStateEntry{
		CredentialName:     "linear-oauth",
		Subject:            "user:alice@example.com",
		PKCEVerifier:       "verifier-xyz",
		MCPServerNamespace: "default",
		MCPServerName:      "linear-mcp",
	})
	require.NoError(t, err)
	assert.Len(t, tok, 32, "16 random bytes hex-encoded → 32 hex chars")

	entry, ok := s.Consume(tok)
	require.True(t, ok)
	assert.Equal(t, "linear-oauth", entry.CredentialName)
	assert.Equal(t, identity.Subject("user:alice@example.com"), entry.Subject)
	assert.Equal(t, "verifier-xyz", entry.PKCEVerifier)
	assert.Equal(t, "default", entry.MCPServerNamespace)
	assert.Equal(t, "linear-mcp", entry.MCPServerName)

	// Single-use: second Consume of the same token fails.
	_, ok = s.Consume(tok)
	assert.False(t, ok)
}

func TestOAuthStateStore_ExpiredEntry(t *testing.T) {
	s := newOAuthStateStore(0) // immediate expiry
	tok, err := s.NewState(oauthStateEntry{
		CredentialName: "x",
		Subject:        "user:a",
	})
	require.NoError(t, err)
	_, ok := s.Consume(tok)
	assert.False(t, ok, "expired entry must not Consume")
}

func TestOAuthStateStore_DistinctTokens(t *testing.T) {
	s := newOAuthStateStore(time.Hour)
	t1, err := s.NewState(oauthStateEntry{CredentialName: "a", Subject: "u:1"})
	require.NoError(t, err)
	t2, err := s.NewState(oauthStateEntry{CredentialName: "b", Subject: "u:2"})
	require.NoError(t, err)
	assert.NotEqual(t, t1, t2, "each NewState must yield a unique token")
}
