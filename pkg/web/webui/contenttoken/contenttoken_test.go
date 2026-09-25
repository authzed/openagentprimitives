package contenttoken_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/contenttoken"
)

func TestSignVerify_RoundTrip(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.Sign(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-1", ArtifactID: "artifact-x", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)
	c, err := s.Verify(raw)
	require.NoError(t, err)
	assert.Equal(t, "ar-1", c.RenderName)
	assert.Equal(t, "default", c.Ns)
	assert.Equal(t, "s1", c.Sess)
}

func TestVerify_RejectsTamperAndExpiry(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, _ := s.Sign(contenttoken.Claims{RenderName: "ar-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := s.Verify(raw + "x")
	assert.Error(t, err, "tampered token must fail")

	expired, _ := s.Sign(contenttoken.Claims{RenderName: "ar-1", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	_, err = s.Verify(expired)
	assert.Error(t, err, "expired token must fail")
}

func TestSignVerifyAsset_RoundTrip(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.SignAsset(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-2", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)
	c, err := s.VerifyAsset(raw)
	require.NoError(t, err)
	assert.Equal(t, "ar-2", c.RenderName)
	assert.Equal(t, "default", c.Ns)
	assert.Equal(t, "s1", c.Sess)
	assert.Equal(t, contenttoken.KindAsset, c.Kind)
}

// TestVerifyAsset_RejectsContentToken is the cross-route replay guard: a
// token minted by Sign (for /content) must not be usable at VerifyAsset
// (/artifacts/a/) even though its signature and expiry are both valid — the
// two routes serve different trust boundaries (the primary vs. a
// server-resolved secondary), so a token scoped to one must never work on
// the other.
func TestVerifyAsset_RejectsContentToken(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.Sign(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)

	_, err = s.VerifyAsset(raw)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "a /content token must be rejected at VerifyAsset")
}

// TestVerify_RejectsAssetToken is the symmetric guard: an asset token must
// not be replayable at the /content route either.
func TestVerify_RejectsAssetToken(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.SignAsset(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-2", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)

	_, err = s.Verify(raw)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "an asset token must be rejected at Verify")
}

func TestVerifyAsset_RejectsTamperAndExpiry(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, _ := s.SignAsset(contenttoken.Claims{RenderName: "ar-2", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := s.VerifyAsset(raw + "x")
	assert.Error(t, err, "tampered asset token must fail")

	expired, _ := s.SignAsset(contenttoken.Claims{RenderName: "ar-2", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	_, err = s.VerifyAsset(expired)
	assert.Error(t, err, "expired asset token must fail")
}

func TestSignVerifyWidget_RoundTrip(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.SignWidget(contenttoken.Claims{Ns: "default", Sess: "s1", ArtifactID: "artifact-w", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)
	c, err := s.VerifyWidget(raw)
	require.NoError(t, err)
	assert.Equal(t, "default", c.Ns)
	assert.Equal(t, "s1", c.Sess)
	assert.Equal(t, "artifact-w", c.ArtifactID)
	assert.Equal(t, contenttoken.KindWidget, c.Kind)
}

// TestVerifyWidget_RejectsContentAndAssetTokens is the cross-route replay
// guard for the widget route: a /content or /artifacts/a/ token must never
// be usable at the script-enabled /mcpui-content route, even though its
// signature and expiry are both valid.
func TestVerifyWidget_RejectsContentAndAssetTokens(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))

	contentTok, err := s.Sign(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-1", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)
	_, err = s.VerifyWidget(contentTok)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "a /content token must be rejected at VerifyWidget")

	assetTok, err := s.SignAsset(contenttoken.Claims{Ns: "default", Sess: "s1", RenderName: "ar-2", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)
	_, err = s.VerifyWidget(assetTok)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "an asset token must be rejected at VerifyWidget")
}

// TestVerify_RejectsWidgetToken is the symmetric guard: a widget token must
// not be replayable at /content or /artifacts/a/ either.
func TestVerify_RejectsWidgetToken(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, err := s.SignWidget(contenttoken.Claims{Ns: "default", Sess: "s1", ArtifactID: "artifact-w", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	require.NoError(t, err)

	_, err = s.Verify(raw)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "a widget token must be rejected at Verify")
	_, err = s.VerifyAsset(raw)
	assert.ErrorIs(t, err, contenttoken.ErrWrongKind, "a widget token must be rejected at VerifyAsset")
}

func TestVerifyWidget_RejectsTamperAndExpiry(t *testing.T) {
	s := contenttoken.New(make([]byte, 32))
	raw, _ := s.SignWidget(contenttoken.Claims{ArtifactID: "artifact-w", ExpiresAt: time.Now().Add(time.Minute).Unix()})
	_, err := s.VerifyWidget(raw + "x")
	assert.Error(t, err, "tampered widget token must fail")

	expired, _ := s.SignWidget(contenttoken.Claims{ArtifactID: "artifact-w", ExpiresAt: time.Now().Add(-time.Minute).Unix()})
	_, err = s.VerifyWidget(expired)
	assert.Error(t, err, "expired widget token must fail")
}
