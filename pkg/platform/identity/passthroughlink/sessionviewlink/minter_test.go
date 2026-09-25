package sessionviewlink

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// TestMinter_HappyPath verifies mint→verify round-trips the sessionRef,
// subject, and backLink, sets Purpose=PurposeSessionView, carries NO
// ArtifactID, and stamps a long (~7 day) TTL — distinct from the 30-minute
// artifact-view link.
func TestMinter_HappyPath(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32),
		passthroughlink.WithIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithAudience(passthroughlink.AudienceIdentityd), // signer default; minter must override to webd
	)
	m := &Minter{Signer: signer}

	before := time.Now()
	subject := identity.RawSubject("user:alice")
	backLink := "https://example.slack.com/archives/C1/p170?thread_ts=170&cid=C1"
	raw, err := m.Mint("default/sess-1", subject, backLink)
	require.NoError(t, err, "Mint should succeed")
	require.NotEmpty(t, raw)

	p, err := signer.Verify(raw,
		passthroughlink.WithExpectedIssuer(passthroughlink.IssuerChannelsd),
		passthroughlink.WithExpectedAudience(passthroughlink.AudienceWebd))
	require.NoError(t, err, "Verify with aud=webd must succeed — minter must override the signer's default audience")

	assert.Equal(t, passthroughlink.PurposeSessionView, p.Purpose, "Purpose round-trips as session_view")
	assert.Equal(t, "default/sess-1", p.SessionRef, "SessionRef round-trips")
	assert.Empty(t, p.ArtifactID, "a session-view link carries no ArtifactID")

	subjectStr, err := subject.Subject()
	require.NoError(t, err)
	assert.Equal(t, subjectStr, p.Subject, "Subject round-trips")
	assert.Equal(t, backLink, p.BackLink, "BackLink round-trips")

	wantExpiry := before.Add(SessionViewLinkTTL)
	gotExpiry := time.Unix(p.ExpiresAt, 0)
	assert.WithinDuration(t, wantExpiry, gotExpiry, time.Minute, "ExpiresAt must be ~7 days out (long TTL, not the 30-min artifact-view TTL)")
}

// TestMinter_SubjectVerified_VerifiedEmail proves a VerifiedEmail principal
// yields SubjectVerified=true on the minted payload.
func TestMinter_SubjectVerified_VerifiedEmail(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{Signer: signer}

	principal := identity.VerifiedEmail("alice@example.com", "")
	raw, err := m.Mint("ns/sess-1", principal, "")
	require.NoError(t, err)

	p, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.True(t, p.SubjectVerified, "VerifiedEmail principal must produce SubjectVerified=true")
}

// TestMinter_SubjectVerified_Unverified proves an unverified principal yields
// SubjectVerified=false.
func TestMinter_SubjectVerified_Unverified(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{Signer: signer}

	principal := identity.FromExternal("local", "", "alice", "").AllowSynthetic()
	raw, err := m.Mint("ns/sess-1", principal, "")
	require.NoError(t, err)

	p, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.False(t, p.SubjectVerified, "unverified principal must produce SubjectVerified=false")
}

// TestMinter_EmptySessionRef_Errors proves an empty sessionRef is rejected at
// mint time — a session-view link with no session to point at is meaningless.
func TestMinter_EmptySessionRef_Errors(t *testing.T) {
	m := &Minter{Signer: passthroughlink.New(make([]byte, 32))}
	_, err := m.Mint("", identity.RawSubject("user:alice"), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sessionRef")
}

// TestMinter_NilSigner_Errors proves an unconfigured minter (nil Signer)
// fails closed rather than panicking.
func TestMinter_NilSigner_Errors(t *testing.T) {
	m := &Minter{}
	_, err := m.Mint("ns/sess", identity.RawSubject("user:alice"), "")
	require.Error(t, err)
}

// TestVerify_RejectsWrongPurpose proves that a link minted for a DIFFERENT
// purpose (artifact_view) never carries Purpose==session_view — Signer.Verify
// itself is purpose-agnostic (a generic HMAC+expiry check), so the
// discriminator a caller (webd's VerifyLink) gates on is this field, and it
// must never collide across purposes.
func TestVerify_RejectsWrongPurpose(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{Signer: signer}

	sessionRaw, err := m.Mint("default/sess-1", identity.RawSubject("user:alice"), "")
	require.NoError(t, err)
	sp, err := signer.Verify(sessionRaw)
	require.NoError(t, err)
	require.Equal(t, passthroughlink.PurposeSessionView, sp.Purpose)

	otherRaw, err := signer.Mint(passthroughlink.Payload{
		Purpose:    passthroughlink.PurposeArtifactView,
		ArtifactID: "art-1",
		SessionRef: "default/sess-1",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	op, err := signer.Verify(otherRaw)
	require.NoError(t, err)
	assert.NotEqual(t, passthroughlink.PurposeSessionView, op.Purpose,
		"an artifact_view link must never be mistakable for a session_view link")
}

// TestVerify_RejectsExpired proves an expired session-view payload is
// rejected by the generic passthroughlink expiry check. The Minter always
// mints a fresh 7-day-out token, so the expired payload is built directly via
// signer.Mint (bypassing the Minter's fixed TTL) to exercise the boundary —
// proving PurposeSessionView links get NO special-cased bypass of ExpiresAt.
func TestVerify_RejectsExpired(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	raw, err := signer.Mint(passthroughlink.Payload{
		Purpose:    passthroughlink.PurposeSessionView,
		SessionRef: "default/sess-1",
		ExpiresAt:  time.Now().Add(-time.Minute).Unix(),
	})
	require.NoError(t, err)

	_, err = signer.Verify(raw)
	require.ErrorIs(t, err, passthroughlink.ErrExpired)
}

// TestVerify_RejectsTamperedSignature proves a bit-flip anywhere in a minted
// session-view token invalidates the HMAC.
func TestVerify_RejectsTamperedSignature(t *testing.T) {
	signer := passthroughlink.New(make([]byte, 32))
	m := &Minter{Signer: signer}
	raw, err := m.Mint("default/sess-1", identity.RawSubject("user:alice"), "")
	require.NoError(t, err)

	tampered := []byte(raw)
	tampered[len(tampered)-1] ^= 0x01
	_, err = signer.Verify(string(tampered))
	require.Error(t, err, "a tampered signature must be rejected")
}
