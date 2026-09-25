// Wiring tests for the channelsd artifact-view minter. The minting logic
// itself is now tested in pkg/platform/identity/passthroughlink/viewlink/minter_test.go;
// this file verifies that newArtifactViewMinter returns a correctly wired
// *viewlink.Minter that satisfies channelkinds.ArtifactViewMinter and that
// the factory's field wiring is correct (signer + webdBaseURL pass through).
package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestNewArtifactViewMinter_SatisfiesInterface verifies that
// newArtifactViewMinter returns a value that satisfies channelkinds.ArtifactViewMinter.
func TestNewArtifactViewMinter_SatisfiesInterface(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := newArtifactViewMinter(signer, func() string { return "https://webd.example.com" })
	var _ channelkinds.ArtifactViewMinter = m
}

// TestNewArtifactViewMinter_HappyPath verifies that the minter built via the
// channelsd factory function produces a correctly shaped URL.
func TestNewArtifactViewMinter_HappyPath(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd), // signer default; minter must override
	)
	base := "https://webd.example.com"
	m := newArtifactViewMinter(signer, func() string { return base })

	artifactID := "art-abc123"
	sessionRef := "default/sess-1"
	subject := identity.RawSubject("user:alice")
	backLink := "https://example.slack.com/archives/C1/p170?thread_ts=170&cid=C1"

	u, err := m.MintArtifactViewLink(artifactID, sessionRef, subject, backLink)
	require.NoError(t, err, "MintArtifactViewLink should succeed")
	assert.True(t, strings.HasPrefix(u, base+"/artifact-view?"), "URL must start with base+/artifact-view?")
	assert.Contains(t, u, "d=", "URL must contain d= query param")
	assert.Contains(t, u, "&sig=", "URL must contain &sig= query param")

	// Reconstruct the raw "<b64>.<sig>" string and verify audience is webd.
	after, ok := strings.CutPrefix(u, base+"/artifact-view?d=")
	require.True(t, ok, "URL must have the expected prefix")
	parts := strings.SplitN(after, "&sig=", 2)
	require.Len(t, parts, 2, "URL must have exactly one d= and one sig= part")
	raw := parts[0] + "." + parts[1]

	p, err := signer.Verify(raw, passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	require.NoError(t, err, "Verify with aud=webd must succeed — audience-override regression guard")
	assert.Equal(t, artifactID, p.ArtifactID)
	assert.Equal(t, passthroughlink.AudienceWebd, p.Audience)
	assert.Equal(t, backLink, p.BackLink)
}

// TestNewArtifactViewMinter_EmptyBaseURL verifies that a missing webd base URL
// returns ("", nil) — a clean skip, not an error.
func TestNewArtifactViewMinter_EmptyBaseURL(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := newArtifactViewMinter(signer, func() string { return "" })

	u, err := m.MintArtifactViewLink("art-1", "ns/sess", identity.RawSubject(""), "")
	require.NoError(t, err, "empty base URL must return nil error (clean skip)")
	assert.Empty(t, u, "empty base URL must return empty URL string")
}
