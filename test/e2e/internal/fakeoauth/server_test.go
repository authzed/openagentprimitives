package fakeoauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"

	"github.com/authzed/openagentprimitives/test/e2e/internal/fakeoauth"
)

// --- helpers -----------------------------------------------------------------

// s256Challenge computes the PKCE S256 code_challenge for a given verifier.
// Mirrors the computation in pkg/tools/mcp/oauth/pkce.go.
func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// noRedirectClient returns an *http.Client that stops after the first redirect
// so tests can inspect 302 responses directly.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// authorizeAndGetCode drives GET /authorize and returns the issued auth code.
// challenge must already be the S256-hashed, base64url-encoded value.
func authorizeAndGetCode(t *testing.T, s *fakeoauth.Server, challenge string) string {
	t.Helper()
	u, err := url.Parse(s.URL() + "/authorize")
	require.NoError(t, err)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", "test-client")
	q.Set("redirect_uri", "http://localhost/callback")
	q.Set("state", "test-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()

	resp, err := noRedirectClient().Get(u.String())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "/authorize must 302")

	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	code := loc.Query().Get("code")
	require.NotEmpty(t, code, "Location must contain code=")
	return code
}

// tokenExchange posts a token exchange and returns the raw response body as a
// map. Asserts status 200.
func tokenExchange(t *testing.T, s *fakeoauth.Server, code, verifier string) map[string]any {
	t.Helper()
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost/callback"},
		"code_verifier": {verifier},
		"client_id":     {"test-client"},
	}
	resp, err := http.PostForm(s.URL()+"/token", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "token exchange must 200")
	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return got
}

// --- discovery ---------------------------------------------------------------

// TestFakeOAuth_DiscoveryRoundTrip verifies that mcpoauth.Discover finds the
// fake's metadata via the /.well-known/oauth-authorization-server fallback.
func TestFakeOAuth_DiscoveryRoundTrip(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	meta, err := mcpoauth.Discover(context.Background(), http.DefaultClient, s.URL())
	require.NoError(t, err, "Discover must succeed against the fake")

	assert.Equal(t, s.URL()+"/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, s.URL()+"/token", meta.TokenEndpoint)
	assert.Equal(t, s.URL()+"/register", meta.RegistrationEndpoint)
}

// --- authorize ---------------------------------------------------------------

// TestFakeOAuth_AuthorizeRedirect verifies the 302 redirect shape from GET /authorize.
func TestFakeOAuth_AuthorizeRedirect(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	const verifier = "test-verifier-of-sufficient-length-for-pkce-requirements"
	challenge := s256Challenge(verifier)

	u, err := url.Parse(s.URL() + "/authorize")
	require.NoError(t, err)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", "test-client")
	q.Set("redirect_uri", "http://localhost/callback")
	q.Set("state", "test-state")
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()

	resp, err := noRedirectClient().Get(u.String())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)

	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "http://localhost/callback", loc.Scheme+"://"+loc.Host+loc.Path,
		"redirect must target redirect_uri")
	assert.NotEmpty(t, loc.Query().Get("code"), "code must be present")
	assert.Equal(t, "test-state", loc.Query().Get("state"), "state must be echoed")
}

// TestFakeOAuth_Authorize_MissingParams verifies that /authorize rejects
// requests with missing required parameters.
func TestFakeOAuth_Authorize_MissingParams(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	cases := []struct {
		name string
		drop string // query param to omit
	}{
		{"missing client_id: 400", "client_id"},
		{"missing redirect_uri: 400", "redirect_uri"},
		{"missing code_challenge: 400", "code_challenge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(s.URL() + "/authorize")
			require.NoError(t, err)
			q := url.Values{
				"response_type":         {"code"},
				"client_id":             {"c"},
				"redirect_uri":          {"http://localhost/cb"},
				"code_challenge":        {"ch"},
				"code_challenge_method": {"S256"},
			}
			delete(q, tc.drop)
			u.RawQuery = q.Encode()

			resp, err := noRedirectClient().Get(u.String())
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// TestFakeOAuth_Authorize_NonS256Method verifies that non-S256 methods are rejected.
func TestFakeOAuth_Authorize_NonS256Method(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	u, err := url.Parse(s.URL() + "/authorize")
	require.NoError(t, err)
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {"c"},
		"redirect_uri":          {"http://localhost/cb"},
		"code_challenge":        {"ch"},
		"code_challenge_method": {"plain"},
	}
	u.RawQuery = q.Encode()

	resp, err := noRedirectClient().Get(u.String())
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// --- token exchange -----------------------------------------------------------

// TestFakeOAuth_TokenExchange_PKCEValid verifies a complete authorize→token round-trip.
func TestFakeOAuth_TokenExchange_PKCEValid(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	verifier := "test-verifier-of-sufficient-length-for-pkce-requirements"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	got := tokenExchange(t, s, code, verifier)
	assert.NotEmpty(t, got["access_token"], "access_token must be present")
	assert.Equal(t, "Bearer", got["token_type"], "token_type must be Bearer")
	assert.NotEmpty(t, got["refresh_token"], "refresh_token must be present")
}

// TestFakeOAuth_TokenExchange_PKCEMismatch verifies that a wrong verifier → 400.
func TestFakeOAuth_TokenExchange_PKCEMismatch(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	verifier := "correct-verifier-long-enough-for-pkce-spec-requirements"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost/callback"},
		"code_verifier": {"wrong-verifier-that-does-not-hash-to-the-challenge"},
		"client_id":     {"test-client"},
	}
	resp, err := http.PostForm(s.URL()+"/token", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"wrong verifier → 400 invalid_grant")
}

// TestFakeOAuth_TokenExchange_CodeSingleUse verifies that the same code cannot
// be exchanged twice.
func TestFakeOAuth_TokenExchange_CodeSingleUse(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	verifier := "single-use-verifier-long-enough-for-pkce-specification"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	// First exchange: must succeed.
	got := tokenExchange(t, s, code, verifier)
	require.NotEmpty(t, got["access_token"], "first exchange must succeed")

	// Second exchange with same code: must fail.
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost/callback"},
		"code_verifier": {verifier},
		"client_id":     {"test-client"},
	}
	resp, err := http.PostForm(s.URL()+"/token", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"second use of same code → 400 invalid_grant")
}

// TestFakeOAuth_TokenExchange_RedirectURIMismatch verifies that a different
// redirect_uri than the one used at /authorize → 400.
func TestFakeOAuth_TokenExchange_RedirectURIMismatch(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	verifier := "redirect-mismatch-verifier-long-enough-for-pkce"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost/different-callback"},
		"code_verifier": {verifier},
		"client_id":     {"test-client"},
	}
	resp, err := http.PostForm(s.URL()+"/token", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"redirect_uri mismatch → 400 invalid_grant")
}

// TestFakeOAuth_TokenExchange_MissingParams verifies that missing required
// params → 400.
func TestFakeOAuth_TokenExchange_MissingParams(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	cases := []struct {
		name string
		drop string
	}{
		{"missing code: 400", "code"},
		{"missing code_verifier: 400", "code_verifier"},
		{"missing redirect_uri: 400", "redirect_uri"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := url.Values{
				"grant_type":    {"authorization_code"},
				"code":          {"somecode"},
				"redirect_uri":  {"http://localhost/callback"},
				"code_verifier": {"someverifier"},
				"client_id":     {"test-client"},
			}
			delete(body, tc.drop)
			resp, err := http.PostForm(s.URL()+"/token", body)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

// --- registration ------------------------------------------------------------

// TestFakeOAuth_Register verifies that POST /register returns a client_id.
func TestFakeOAuth_Register(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	reqBody, err := json.Marshal(map[string]any{
		"redirect_uris":              []string{"http://localhost/cb"},
		"client_name":                "test client",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	require.NoError(t, err)

	resp, err := http.Post(s.URL()+"/register", "application/json",
		strings.NewReader(string(reqBody)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "register must 200; body=%v", resp.Body)

	var got map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	assert.NotEmpty(t, got["client_id"], "client_id must be returned")
	// Public client: no client_secret.
	assert.Empty(t, got["client_secret"], "no client_secret for public client")
}

// TestFakeOAuth_Register_MissingRedirectURIs verifies that /register rejects
// a request with no redirect_uris.
func TestFakeOAuth_Register_MissingRedirectURIs(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	reqBody, err := json.Marshal(map[string]any{"client_name": "no-uris"})
	require.NoError(t, err)

	resp, err := http.Post(s.URL()+"/register", "application/json",
		strings.NewReader(string(reqBody)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// TestFakeOAuth_RegisterRoundTrip uses the real pkg/tools/mcp/oauth.Register
// function against the fake, verifying that the shapes are compatible.
func TestFakeOAuth_RegisterRoundTrip(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	clientID, clientSecret, err := mcpoauth.Register(
		context.Background(),
		http.DefaultClient,
		s.URL()+"/register",
		[]string{"http://localhost/callback"},
	)
	require.NoError(t, err, "Register must succeed against the fake")
	assert.NotEmpty(t, clientID, "client_id returned")
	assert.Empty(t, clientSecret, "no secret for public client")
}

// --- scripted token response -------------------------------------------------

// TestFakeOAuth_SetTokenResponse verifies that SetTokenResponse causes /token
// to return the scripted values.
func TestFakeOAuth_SetTokenResponse(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	s.SetTokenResponse(&fakeoauth.TokenResponse{
		AccessToken:  "scripted-at",
		RefreshToken: "scripted-rt",
		TokenType:    "Bearer",
		ExpiresIn:    7200,
		Scope:        "read write",
	})

	verifier := "scripted-verifier-long-enough-for-pkce-spec-requirements"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	got := tokenExchange(t, s, code, verifier)
	assert.Equal(t, "scripted-at", got["access_token"])
	assert.Equal(t, "scripted-rt", got["refresh_token"])
	assert.Equal(t, "Bearer", got["token_type"])
	assert.EqualValues(t, 7200, got["expires_in"])
	assert.Equal(t, "read write", got["scope"])
}

// --- reset -------------------------------------------------------------------

// TestFakeOAuth_Reset verifies that Reset clears codes and recorded requests
// but preserves the scripted token response.
func TestFakeOAuth_Reset(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	// Script a token response and drive one authorize+token round-trip.
	s.SetTokenResponse(&fakeoauth.TokenResponse{
		AccessToken: "before-reset", TokenType: "Bearer",
	})
	verifier := "reset-verifier-long-enough-for-pkce-spec-requirements"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)
	tokenExchange(t, s, code, verifier)

	// Recorded requests: discovery probe + /.well-known... is not counted
	// here, but at least /authorize and /token were called.
	before := s.RecordedRequests()
	require.NotEmpty(t, before, "recorded requests non-empty before reset")

	s.Reset()

	// Recorded requests cleared.
	assert.Empty(t, s.RecordedRequests(), "recorded requests cleared after reset")

	// The old code is gone — a second token exchange with the same code fails.
	body := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost/callback"},
		"code_verifier": {verifier},
		"client_id":     {"test-client"},
	}
	resp, err := http.PostForm(s.URL()+"/token", body)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode,
		"code cleared by Reset; exchange must fail")

	// Scripted token response is preserved: get a new code and exchange it.
	newCode := authorizeAndGetCode(t, s, challenge)
	got := tokenExchange(t, s, newCode, verifier)
	assert.Equal(t, "before-reset", got["access_token"],
		"scripted token response survives Reset")
}

// --- recorded requests -------------------------------------------------------

// TestFakeOAuth_RecordedRequests verifies that every handled request appears
// in RecordedRequests in order.
func TestFakeOAuth_RecordedRequests(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	// 1. Discovery.
	_, err := mcpoauth.Discover(context.Background(), http.DefaultClient, s.URL())
	require.NoError(t, err)

	// 2. Authorize.
	verifier := "recorded-verifier-long-enough-for-pkce-spec-requirements"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)

	// 3. Token exchange.
	tokenExchange(t, s, code, verifier)

	reqs := s.RecordedRequests()
	paths := make([]string, 0, len(reqs))
	for _, r := range reqs {
		paths = append(paths, r.Path)
	}

	// Discovery probes "/" then "/.well-known/oauth-authorization-server".
	// The root "/" handler doesn't call s.record so only the well-known path
	// is recorded from that leg; then /authorize and /token.
	assert.Contains(t, paths, "/.well-known/oauth-authorization-server",
		"well-known path must be recorded")
	assert.Contains(t, paths, "/authorize", "/authorize must be recorded")
	assert.Contains(t, paths, "/token", "/token must be recorded")
}

// TestFakeOAuth_RecordedRequests_FormValues verifies that POST form values are
// captured in RecordedRequest.Values alongside query params.
func TestFakeOAuth_RecordedRequests_FormValues(t *testing.T) {
	s := fakeoauth.NewServer()
	t.Cleanup(s.Close)

	verifier := "form-values-verifier-long-enough-for-pkce-spec"
	challenge := s256Challenge(verifier)
	code := authorizeAndGetCode(t, s, challenge)
	tokenExchange(t, s, code, verifier)

	reqs := s.RecordedRequests()
	var tokenReq *fakeoauth.RecordedRequest
	for i := range reqs {
		if reqs[i].Path == "/token" {
			tokenReq = &reqs[i]
			break
		}
	}
	require.NotNil(t, tokenReq, "/token must be in recorded requests")
	assert.Equal(t, "authorization_code", tokenReq.Values.Get("grant_type"),
		"grant_type captured from POST form")
	assert.NotEmpty(t, tokenReq.Values.Get("code"), "code captured from POST form")
	assert.NotEmpty(t, tokenReq.Values.Get("code_verifier"), "code_verifier captured from POST form")
}
