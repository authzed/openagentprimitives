package viewlink

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestMinter_DownloadLink verifies the direct-download link points at the
// /artifact-download endpoint yet carries the same view-authorized payload as
// the live-view link (download is a presentation of the same view grant).
func TestMinter_DownloadLink(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd))
	base := "https://webd.example.com"
	m := &Minter{Signer: signer, WebdBaseURL: func() string { return base }}

	u, err := m.MintArtifactDownloadLink("art-abc", "default/sess-1", identity.RawSubject("user:alice"), "")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(u, base+"/artifact-download?d="), "must point at the download endpoint")
	assert.Contains(t, u, "&sig=")

	after, ok := strings.CutPrefix(u, base+"/artifact-download?d=")
	require.True(t, ok)
	parts := strings.SplitN(after, "&sig=", 2)
	require.Len(t, parts, 2)
	p, err := signer.Verify(parts[0]+"."+parts[1], passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	require.NoError(t, err)
	assert.Equal(t, "art-abc", p.ArtifactID)
	assert.Equal(t, passthroughlink.PurposeArtifactView, p.Purpose, "same view-authorized purpose")
	assert.Equal(t, "default/sess-1", p.SessionRef)
}

// TestMinter_DownloadLink_EmptyBaseSkips: an unconfigured webd base URL yields a
// clean ("", nil) skip, same as the view link — the desktop case where no
// external origin is wired yet.
func TestMinter_DownloadLink_EmptyBaseSkips(t *testing.T) {
	m := &Minter{Signer: passthroughlink.New(make([]byte, 32)), WebdBaseURL: func() string { return "" }}
	u, err := m.MintArtifactDownloadLink("art-abc", "default/sess-1", identity.RawSubject("user:alice"), "")
	require.NoError(t, err)
	assert.Empty(t, u)
}

// TestMinter_HappyPath verifies that the minter produces a correctly shaped
// URL and that the embedded payload verifies back to the original ArtifactID
// with aud=webd (regression guard for the audience-override fix).
func TestMinter_HappyPath(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd), // signer default; minter must override
	)
	base := "https://webd.example.com"
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return base },
	}

	artifactID := "art-abc123"
	sessionRef := "default/sess-1"
	subject := identity.RawSubject("user:alice")

	backLink := "https://example.slack.com/archives/C1/p170?thread_ts=170&cid=C1"
	u, err := m.MintArtifactViewLink(artifactID, sessionRef, subject, backLink)
	require.NoError(t, err, "MintArtifactViewLink should succeed")
	assert.True(t, strings.HasPrefix(u, base+"/artifact-view?"), "URL must start with base+/artifact-view?")
	assert.Contains(t, u, "d=", "URL must contain d= query param")
	assert.Contains(t, u, "&sig=", "URL must contain &sig= query param")

	// Reconstruct the raw "<b64>.<sig>" string from the query params and verify.
	after, ok := strings.CutPrefix(u, base+"/artifact-view?d=")
	require.True(t, ok, "URL must have the expected prefix")
	parts := strings.SplitN(after, "&sig=", 2)
	require.Len(t, parts, 2, "URL must have exactly one d= and one sig= part")
	raw := parts[0] + "." + parts[1]

	p, err := signer.Verify(raw, passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	require.NoError(t, err, "Verify with aud=webd must succeed — regression guard for audience-override")
	assert.Equal(t, artifactID, p.ArtifactID, "ArtifactID round-trips through Verify")
	assert.Equal(t, passthroughlink.PurposeArtifactView, p.Purpose, "Purpose is artifact_view")
	assert.Equal(t, sessionRef, p.SessionRef, "SessionRef round-trips")
	subjectStr, err := subject.Subject()
	require.NoError(t, err)
	assert.Equal(t, subjectStr, p.Subject, "Subject round-trips")
	// Explicit aud assertion: the minter must set aud=webd even though the
	// signer was constructed with WithAudience(AudienceIdentityd).
	assert.Equal(t, passthroughlink.AudienceWebd, p.Audience, "Audience must be webd, not identityd")
	assert.Equal(t, backLink, p.BackLink, "BackLink round-trips through the minted link")
}

// TestMinter_SubjectVerified_VerifiedEmail verifies that minting with a
// VerifiedEmail principal sets SubjectVerified=true and encodes the correct
// Subject canonical in the payload.
func TestMinter_SubjectVerified_VerifiedEmail(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	base := "https://webd.example.com"
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return base },
	}

	principal := identity.VerifiedEmail("alice@example.com", "")
	u, err := m.MintArtifactViewLink("art-1", "ns/sess-1", principal, "")
	require.NoError(t, err)

	after, ok := strings.CutPrefix(u, base+"/artifact-view?d=")
	require.True(t, ok)
	parts := strings.SplitN(after, "&sig=", 2)
	require.Len(t, parts, 2)
	raw := parts[0] + "." + parts[1]

	p, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.True(t, p.SubjectVerified, "VerifiedEmail principal must produce SubjectVerified=true")
	subjectStr, err := principal.Subject()
	require.NoError(t, err)
	assert.Equal(t, subjectStr, p.Subject, "Subject must be user:<base64(alice@example.com)>")
}

// TestMinter_SubjectVerified_Unverified verifies that minting with an
// unverified (FromExternal, no email) principal sets SubjectVerified=false.
func TestMinter_SubjectVerified_Unverified(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	base := "https://webd.example.com"
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return base },
	}

	principal := identity.FromExternal("local", "", "alice", "").AllowSynthetic()
	u, err := m.MintArtifactViewLink("art-1", "ns/sess-1", principal, "")
	require.NoError(t, err)

	after, ok := strings.CutPrefix(u, base+"/artifact-view?d=")
	require.True(t, ok)
	parts := strings.SplitN(after, "&sig=", 2)
	require.Len(t, parts, 2)
	raw := parts[0] + "." + parts[1]

	p, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.False(t, p.SubjectVerified, "unverified principal must produce SubjectVerified=false")
}

// TestMinter_EmptyBaseURL verifies that a missing webd base URL returns
// ("", nil) — a clean skip, not an error.
func TestMinter_EmptyBaseURL(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return "" },
	}

	u, err := m.MintArtifactViewLink("art-1", "ns/sess", identity.RawSubject(""), "")
	require.NoError(t, err, "empty base URL must return nil error (clean skip)")
	assert.Empty(t, u, "empty base URL must return empty URL string")
}

// TestMinter_TrailingSlash verifies that a trailing slash on the base URL is
// stripped — the produced URL must not contain "//".
func TestMinter_TrailingSlash(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return "https://webd.example.com/" },
	}

	u, err := m.MintArtifactViewLink("art-1", "ns/sess", identity.RawSubject("").AllowSynthetic(), "")
	require.NoError(t, err)
	assert.False(t, strings.Contains(u, "//artifact-view"), "double-slash must be stripped; got %q", u)
}

// TestMinter_EmptyArtifactID verifies that an empty artifactID surfaces an
// error.
func TestMinter_EmptyArtifactID(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{
		Signer:      signer,
		WebdBaseURL: func() string { return "https://webd.example.com" },
	}

	_, err := m.MintArtifactViewLink("", "ns/sess", identity.RawSubject(""), "")
	require.Error(t, err, "empty artifactID must return an error")
	assert.Contains(t, err.Error(), "artifactID")
}
