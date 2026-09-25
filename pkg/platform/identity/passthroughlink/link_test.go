package passthroughlink

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// signSansJTI produces a valid token in the package wire format
// (base64url(json).hex(hmac)) for a payload with NO jti, so a jti-less but
// otherwise well-signed token can be exercised against Verify. It mirrors
// Mint's signing steps but skips the jti auto-fill.
func signSansJTI(t *testing.T, key []byte, p Payload) string {
	t.Helper()
	p.JTI = "" // explicit: this token deliberately omits jti
	body, err := json.Marshal(p)
	require.NoError(t, err, "marshal jti-less payload")
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(b64))
	return b64 + "." + hex.EncodeToString(mac.Sum(nil))
}

func TestMintAndVerify_RoundTrip(t *testing.T) {
	key := []byte("test-signing-key-32-bytes-long!")
	signer := New(key)

	p := Payload{
		SessionRef:          "default/s1",
		Subject:             "user:alice",
		RequiredCredentials: []string{"github-pat", "linear-oauth"},
		ExpiresAt:           time.Now().Add(30 * time.Minute).Unix(),
	}
	raw, err := signer.Mint(p)
	require.NoError(t, err)

	got, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, p.SessionRef, got.SessionRef)
	assert.Equal(t, p.Subject, got.Subject)
	assert.Equal(t, p.RequiredCredentials, got.RequiredCredentials)
	assert.Equal(t, p.Purpose, got.Purpose)
	assert.Equal(t, p.ExpiresAt, got.ExpiresAt)
}

func TestMintAndVerify_PortalPayload(t *testing.T) {
	signer := New([]byte("k"))
	in := Payload{
		Subject:   "user:alice",
		Purpose:   "portal",
		ExpiresAt: time.Now().Add(10 * time.Minute).Unix(),
	}
	raw, err := signer.Mint(in)
	require.NoError(t, err)
	got, err := signer.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, "portal", got.Purpose)
	assert.Empty(t, got.SessionRef)
	assert.Empty(t, got.RequiredCredentials)
}

func TestVerify_RejectsTamperedPayload(t *testing.T) {
	signer := New([]byte("k"))
	raw, err := signer.Mint(Payload{SessionRef: "default/s1", Subject: "user:alice", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	// Flip one byte in the encoded payload (everything before the trailing ".<sig>").
	// The byte-flip may land in the base64 chunk (caught by ErrInvalidSignature
	// since the recomputed MAC won't match) or, if a base64-illegal byte lands
	// past the signature boundary, in the sig itself (still ErrInvalidSignature).
	// Either way we expect an error — we don't pin the specific sentinel.
	idx := len(raw) - 10
	tampered := []byte(raw)
	tampered[idx] ^= 0x01
	_, err = signer.Verify(string(tampered))
	require.Error(t, err)
}

func TestVerify_RejectsWrongSignature(t *testing.T) {
	a := New([]byte("key-A"))
	b := New([]byte("key-B"))
	raw, err := a.Mint(Payload{SessionRef: "default/s1", Subject: "user:alice", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	_, err = b.Verify(raw)
	require.ErrorIs(t, err, ErrInvalidSignature)
}

func TestVerify_RejectsExpired(t *testing.T) {
	signer := New([]byte("k"))
	raw, err := signer.Mint(Payload{SessionRef: "default/s1", Subject: "user:alice", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	require.NoError(t, err)
	_, err = signer.Verify(raw)
	require.ErrorIs(t, err, ErrExpired)
}

// TestMint_AutoPopulatesClaims pins the auto-fill behavior added when
// the Payload picked up JWT-style iss/aud/iat/nbf/jti claims. Callers
// can leave them zero — Mint fills them from the Signer's config.
func TestMint_AutoPopulatesClaims(t *testing.T) {
	signer := New([]byte("k"),
		WithIssuer(IssuerChannelsd),
		WithAudience(AudienceIdentityd))
	before := time.Now().Unix()
	raw, err := signer.Mint(Payload{
		SessionRef: "default/s1",
		Subject:    "user:alice",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	got, err := signer.Verify(raw)
	require.NoError(t, err)

	assert.Equal(t, IssuerChannelsd, got.Issuer, "iss auto-filled from Signer config")
	assert.Equal(t, AudienceIdentityd, got.Audience, "aud auto-filled from Signer config")
	assert.GreaterOrEqual(t, got.IssuedAt, before, "iat is now-ish")
	assert.LessOrEqual(t, got.IssuedAt, time.Now().Unix()+1, "iat is not in the future")
	assert.LessOrEqual(t, got.NotBefore, got.IssuedAt, "nbf must be ≤ iat (built with negative skew)")
	require.NotEmpty(t, got.JTI, "jti must be auto-populated")
	// 16 random bytes base64url-encoded → 22 chars without padding.
	assert.Len(t, got.JTI, 22, "jti must be 16 random bytes base64url-encoded")
}

func TestVerify_RejectsExpectedIssuerMismatch(t *testing.T) {
	signer := New([]byte("k"), WithIssuer(IssuerChannelsd))
	raw, err := signer.Mint(Payload{
		Subject:   "user:alice",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	// Same key, but the verifier expects a different issuer.
	_, err = signer.Verify(raw, WithExpectedIssuer(IssuerIdentityd))
	require.ErrorIs(t, err, ErrWrongIssuer,
		"a channelsd-minted link verified with WithExpectedIssuer(identityd) must reject — prevents cookie-vs-deeplink confusion")
}

func TestVerify_RejectsExpectedAudienceMismatch(t *testing.T) {
	signer := New([]byte("k"), WithAudience(AudienceIdentityd))
	raw, err := signer.Mint(Payload{
		Subject:   "user:alice",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	_, err = signer.Verify(raw, WithExpectedAudience("some-other-system"))
	require.ErrorIs(t, err, ErrWrongAudience)
}

func TestVerify_RejectsIssuedInFuture(t *testing.T) {
	// Build a Signer with a manipulated "now" that mints a payload
	// claiming to be from 10 minutes from real-now. A vanilla
	// verifier (real time) must reject as iat-in-future.
	mintSigner := New([]byte("k"))
	mintSigner.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	raw, err := mintSigner.Mint(Payload{
		Subject:   "user:alice",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	verifier := New([]byte("k")) // real clock
	_, err = verifier.Verify(raw)
	require.ErrorIs(t, err, ErrIssuedInFuture)
}

func TestVerify_RejectsNotBeforeInFuture(t *testing.T) {
	// Manually set NotBefore in the far future on an otherwise-valid
	// payload. Mint's auto-fill only runs when the input field is
	// zero, so this passes through.
	signer := New([]byte("k"))
	raw, err := signer.Mint(Payload{
		Subject:   "user:alice",
		NotBefore: time.Now().Add(10 * time.Minute).Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	_, err = signer.Verify(raw)
	require.ErrorIs(t, err, ErrNotYetValid)
}

func TestVerify_ToleratesClockSkew(t *testing.T) {
	// nbf just inside skew → accepted.
	signer := New([]byte("k"), WithClockSkew(60*time.Second))
	raw, err := signer.Mint(Payload{
		Subject:   "user:alice",
		NotBefore: time.Now().Add(45 * time.Second).Unix(), // 45s in the future, within 60s skew
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	_, err = signer.Verify(raw)
	assert.NoError(t, err, "nbf within skew window must be accepted to tolerate normal NTP drift")
}

func TestMintVerify_ArtifactViewLink(t *testing.T) {
	key := make([]byte, 32)
	s := New(key,
		WithIssuer(IssuerChannelsd),
		WithAudience(AudienceWebd))
	raw, err := s.Mint(Payload{
		Purpose: PurposeArtifactView, ArtifactID: "artifact-abc",
		SessionRef: "default/sess1", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	p, err := s.Verify(raw,
		WithExpectedIssuer(IssuerChannelsd),
		WithExpectedAudience(AudienceWebd))
	require.NoError(t, err)
	assert.Equal(t, "artifact-abc", p.ArtifactID)
	assert.Equal(t, PurposeArtifactView, p.Purpose)
}

// TestMintVerify_BackLinkRoundTrips guards the optional, opaque BackLink
// claim used by artifact_view links to carry a "back to origin" URL.
func TestMintVerify_BackLinkRoundTrips(t *testing.T) {
	s := New(make([]byte, 32))
	const back = "https://example.slack.com/archives/C1/p1700000000000100?thread_ts=1700000000.000100&cid=C1"
	raw, err := s.Mint(Payload{
		Purpose:    PurposeArtifactView,
		ArtifactID: "art-1",
		SessionRef: "ns/sess",
		BackLink:   back,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err, "Mint")

	p, err := s.Verify(raw)
	require.NoError(t, err, "Verify")
	assert.Equal(t, back, p.BackLink, "BackLink round-trips through Mint→Verify")

	// Empty BackLink must be omitted from the wire (omitempty).
	raw2, err := s.Mint(Payload{ExpiresAt: time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err, "Mint without BackLink")
	p2, err := s.Verify(raw2)
	require.NoError(t, err, "Verify without BackLink")
	assert.Empty(t, p2.BackLink, "absent BackLink decodes to empty string")
}

// TestVerify_RejectsMissingJTI guards that a signed token whose payload lacks
// a jti is rejected. Mint always sets jti, so a jti-less token cannot have come
// from a legitimate minter (a buggy/forged minter, or a hand-rolled token). We
// craft the wire token directly with the package's own HMAC so the signature is
// valid — proving the rejection is on the jti requirement, not the signature.
func TestVerify_RejectsMissingJTI(t *testing.T) {
	key := []byte("test-signing-key-32-bytes-long!!")
	signer := New(key)

	// Mint produces a token WITH jti; strip it by re-signing a jti-less body
	// using the same canonical wire format (base64url(json).hex(hmac)).
	raw := signSansJTI(t, key, Payload{
		SessionRef: "default/s1",
		Subject:    "user:alice",
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})

	_, err := signer.Verify(raw)
	require.ErrorIs(t, err, ErrMissingJTI,
		"a validly-signed token without a jti must be rejected — no legit minter omits it")
}

// TestVerify_AcceptsPresentJTI is the positive control for the jti requirement:
// a normally-minted token (jti auto-populated) verifies cleanly.
func TestVerify_AcceptsPresentJTI(t *testing.T) {
	signer := New([]byte("k"))
	raw, err := signer.Mint(Payload{
		Subject:   "user:alice",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err, "Mint")
	got, err := signer.Verify(raw)
	require.NoError(t, err, "Verify")
	assert.NotEmpty(t, got.JTI, "minted token carries a jti and verifies")
}

// TestSigner_WorkshopCredentialPayload guards the workshop-credential deep
// link: it reuses the existing SessionRef (the builder session) and
// RequiredCredentials fields, and carries the new AgentIdentityRef
// ("<workshopNamespace>/<agentIdentityName>") through Mint→Verify intact.
func TestSigner_WorkshopCredentialPayload(t *testing.T) {
	s := New([]byte("k0k0k0k0k0k0k0k0k0k0k0k0k0k0k0k0"))
	raw, err := s.Mint(Payload{
		Purpose:             PurposeWorkshopCredential,
		SessionRef:          "builder-b/builder-x",
		AgentIdentityRef:    "ws-abc123/weather-ai",
		RequiredCredentials: []string{"api_key"},
		ExpiresAt:           time.Now().Add(time.Hour).Unix(),
		JTI:                 "j1",
	})
	require.NoError(t, err)
	got, err := s.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, PurposeWorkshopCredential, got.Purpose)
	assert.Equal(t, "ws-abc123/weather-ai", got.AgentIdentityRef)
	assert.Equal(t, []string{"api_key"}, got.RequiredCredentials)
}

// TestPayload_SubjectVerified_BackwardCompat guards that a payload JSON that
// predates the SubjectVerified field (no "subVerified" key) deserializes
// to SubjectVerified==false. This ensures old signed links never silently
// gain the verified shortcut via deserialization.
func TestPayload_SubjectVerified_BackwardCompat(t *testing.T) {
	// Raw JSON produced by a minter that didn't know about subVerified.
	// The field is absent (omitempty means false is not serialized).
	oldPayloadJSON := `{"sub":"user:alice","exp":9999999999,"iat":1000,"jti":"abc"}`
	var p Payload
	require.NoError(t, json.Unmarshal([]byte(oldPayloadJSON), &p), "unmarshal old payload")
	assert.False(t, p.SubjectVerified, "SubjectVerified must be false when absent from JSON (backward compat: old links never shortcut)")
	assert.Equal(t, identity.Subject("user:alice"), p.Subject)
}
