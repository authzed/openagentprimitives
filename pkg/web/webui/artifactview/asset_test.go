package artifactview_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui/contenttoken"
)

// TestAsset_ValidToken_ServesBytesAndMIME is the happy path: a valid asset
// token verifies to (ns, sess, renderName), the handler fetches that
// render's bytes, and serves them inline (no Content-Disposition) with the
// render's MIME.
func TestAsset_ValidToken_ServesBytesAndMIME(t *testing.T) {
	av := fakeAV()
	var gotNs, gotSess, gotRender string
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		require.Equal(t, "TOK123", token)
		return "default", "s1", "ar-secondary", nil
	}
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		gotNs, gotSess, gotRender = ns, sess, renderName
		return []byte{0x89, 'P', 'N', 'G'}, "image/png", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct=TOK123"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []byte{0x89, 'P', 'N', 'G'}, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Empty(t, rec.Header().Get("Content-Disposition"), "an inline <img>/<link> fetch must not force a download")
	assert.Equal(t, "default", gotNs)
	assert.Equal(t, "s1", gotSess)
	assert.Equal(t, "ar-secondary", gotRender)
}

// TestAsset_TamperedToken_Forbidden: a token that fails verification (bad
// signature, expired, wrong kind — all surfaced as an error by
// VerifyAssetToken) must 403, never serve bytes.
func TestAsset_TamperedToken_Forbidden(t *testing.T) {
	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		return "", "", "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct=tampered"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAsset_AbsentToken_Forbidden: no ?ct at all must also 403 (an empty
// token string is just another verification failure).
func TestAsset_AbsentToken_Forbidden(t *testing.T) {
	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		assert.Empty(t, token, "no ?ct query param means an empty token is what's verified")
		return "", "", "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/"))

	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestAsset_ContentTokenRejected_Forbidden is the cross-route replay guard
// exercised end to end through the REAL contenttoken package (not a fake
// stand-in): a token minted for the /content route (Sign, Kind=content) is
// well-signed and unexpired, yet must still be rejected by
// contenttoken.VerifyAsset — and thus by the /artifacts/a/ handler — because
// it was never minted as an asset token. A stolen or replayed /content token
// (e.g. from a browser's iframe src) must never double as an asset-fetch
// credential for an unrelated secondary render.
func TestAsset_ContentTokenRejected_Forbidden(t *testing.T) {
	signer := contenttoken.New(make([]byte, 32))
	contentTok, err := signer.Sign(contenttoken.Claims{
		Ns: "default", Sess: "s1", RenderName: "ar-primary",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		c, err := signer.VerifyAsset(token)
		return c.Ns, c.Sess, c.RenderName, err
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct="+contentTok))

	assert.Equal(t, http.StatusForbidden, rec.Code, "a /content token must never work at /artifacts/a/")
}

// TestAsset_RealAssetToken_RoundTrips is the positive counterpart: a token
// genuinely minted by SignAsset DOES verify and serve at /artifacts/a/,
// proving the rejection above is about Kind, not about the fake always
// failing.
func TestAsset_RealAssetToken_RoundTrips(t *testing.T) {
	signer := contenttoken.New(make([]byte, 32))
	assetTok, err := signer.SignAsset(contenttoken.Claims{
		Ns: "default", Sess: "s1", RenderName: "ar-secondary",
		ExpiresAt: time.Now().Add(time.Minute).Unix(),
	})
	require.NoError(t, err)

	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		c, err := signer.VerifyAsset(token)
		return c.Ns, c.Sess, c.RenderName, err
	}
	var gotRender string
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		gotRender = renderName
		return []byte("body{}"), "text/css", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct="+assetTok))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "ar-secondary", gotRender)
}

// TestAsset_FetchRenderError_BadGateway mirrors TestContent_FetchRenderError_BadGateway:
// a verified token whose render is unavailable is a 502, not a 200 with empty body.
func TestAsset_FetchRenderError_BadGateway(t *testing.T) {
	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		return "default", "s1", "ar-secondary", nil
	}
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return nil, "", assert.AnError
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct=TOK123"))

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestAsset_TextMIME_ForcesUTF8Charset mirrors TestContent_TextHTML_ForcesUTF8Charset:
// a secondary served as text/* gets an explicit charset so nosniff doesn't
// cause a Latin-1 fallback mojibake.
func TestAsset_TextMIME_ForcesUTF8Charset(t *testing.T) {
	av := fakeAV()
	av.verifyAssetToken = func(token string) (string, string, string, error) {
		return "default", "s1", "ar-secondary", nil
	}
	av.fetchRender = func(ctx context.Context, ns, sess, renderName string) ([]byte, string, error) {
		return []byte("body { color: #333 }"), "text/css", nil
	}
	s := newServer(t, "user:abc", av)

	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req(sandboxHost, "/artifacts/a/?ct=TOK123"))

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "text/css; charset=utf-8", rec.Header().Get("Content-Type"))
}
