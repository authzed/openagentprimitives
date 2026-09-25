package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestNewArtifactViewMinter_MintsLinkWebdCanVerify proves the minter webd wires
// into the builtin chat mints artifact-view links that webd's OWN verifier
// accepts: issuer=channelsd, audience=webd, purpose=artifact_view (exactly what
// artifactViewDeps.VerifyLink checks). Reusing webd's identityd-issuer
// cookieSigner would mint links webd then rejects — a subtle break this locks
// down.
func TestNewArtifactViewMinter_MintsLinkWebdCanVerify(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	const base = "https://webd.example"
	m := newArtifactViewMinter(key, func() string { return base })
	require.NotNil(t, m, "a non-empty key must yield a minter")

	link, err := m.MintArtifactViewLink("art-1", "default/sess-1", identity.RawSubject("user:alice"), "")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(link, base+"/artifact-view?"), "link = %s", link)

	u, err := url.Parse(link)
	require.NoError(t, err)
	d, sig := u.Query().Get("d"), u.Query().Get("sig")
	require.NotEmpty(t, d)
	require.NotEmpty(t, sig)

	// Reconstruct the "<b64>.<sig>" token webd's handler reassembles from
	// ?d= + ?sig= and verify it exactly as artifactViewDeps.VerifyLink does.
	verifier := passthroughlink.New(key)
	p, err := verifier.Verify(d+"."+sig,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	require.NoError(t, err, "webd's own verifier must accept the minted link")
	assert.Equal(t, passthroughlink.PurposeArtifactView, p.Purpose)
	assert.Equal(t, "art-1", p.ArtifactID)
	assert.Equal(t, "user:alice", p.Subject.String())
}

// TestNewArtifactViewMinter_NilKey_ReturnsNil proves that with no signing key
// the minter is nil (browser view links genuinely unavailable). The builtin
// live_view_offer sender then surfaces that loudly rather than dropping the
// agent's offers silently.
func TestNewArtifactViewMinter_NilKey_ReturnsNil(t *testing.T) {
	assert.Nil(t, newArtifactViewMinter(nil, func() string { return "https://webd.example" }))
	assert.Nil(t, newArtifactViewMinter([]byte{}, func() string { return "https://webd.example" }))
}
