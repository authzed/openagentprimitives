package identityd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/accesstoken"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// fakeMinter is the AccessTokenMinter test double: a canned Minted/error,
// recording every MintParams it receives in call order.
type fakeMinter struct {
	calls  []MintParams
	minted Minted
	err    error
}

func (f *fakeMinter) MintAccessToken(_ context.Context, p MintParams) (Minted, error) {
	f.calls = append(f.calls, p)
	if f.err != nil {
		return Minted{}, f.err
	}
	return f.minted, nil
}

// newTestServerWithMinter is newTestServerWithConsent plus a stubbed
// AccessTokenMinter, so /oauth/token (gated on Consent, same as
// authorize/consent) can be exercised without a real SpiceDB/K8s round trip.
func newTestServerWithMinter(t *testing.T, m AccessTokenMinter) *Server {
	t.Helper()
	c := fake.NewClientBuilder().Build()
	return NewServer(Deps{
		K8s:             c,
		LinkSigner:      passthroughlink.New([]byte("test-key")),
		ExternalBaseURL: func() string { return "https://example.org" },
		Consent: stubConsentDeps{classes: []ConsentClass{
			{ID: "default/demo-agent", DisplayName: "Demo"},
		}},
		Minter: m,
	})
}

func doTokenPost(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestTokenEndpoint(t *testing.T) {
	mintedValue := accesstoken.TokenPrefix + "test-minted-value"
	minter := &fakeMinter{minted: Minted{
		Value:     mintedValue,
		TokenID:   "at-test",
		ExpiresAt: time.Now().Add(90 * 24 * time.Hour),
	}}
	s := newTestServerWithMinter(t, minter)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	otherClient := mintTestClientID(t, s, "other tool", []string{"http://127.0.0.1:8888/cb"})
	subject := identity.CanonicalFromTrusted("alice", "test fixture")

	// newCode mints a fresh single-use authorization code bound to a fresh
	// PKCE pair, and returns the matching baseline form for /oauth/token.
	newCode := func(t *testing.T) url.Values {
		t.Helper()
		p, err := oauth.NewPKCE()
		require.NoError(t, err)
		code, err := s.authCodes.NewCode(authCodeEntry{
			ClientID:     validClient,
			RedirectURI:  "http://127.0.0.1:7777/cb",
			Challenge:    p.Challenge,
			Subject:      subject,
			Role:         accesstoken.RoleRead,
			ScopeClasses: []string{"default/demo-agent"},
		})
		require.NoError(t, err)
		return url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {"http://127.0.0.1:7777/cb"},
			"client_id":     {validClient},
			"code_verifier": {p.Verifier},
		}
	}

	cases := []struct {
		name          string
		mutate        func(f url.Values) url.Values
		exchangeTwice bool
		wantCode      int
		wantErr       string
	}{
		{
			name:     "happy exchange: 200 + bearer",
			mutate:   func(f url.Values) url.Values { return f },
			wantCode: http.StatusOK,
		},
		{
			name: "wrong grant_type: 400 unsupported_grant_type",
			mutate: func(f url.Values) url.Values {
				f.Set("grant_type", "client_credentials")
				return f
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "unsupported_grant_type",
		},
		{
			name: "unknown code: 400 invalid_grant",
			mutate: func(f url.Values) url.Values {
				f.Set("code", "does-not-exist")
				return f
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name:          "reused code: 400 invalid_grant",
			mutate:        func(f url.Values) url.Values { return f },
			exchangeTwice: true,
			wantCode:      http.StatusBadRequest,
			wantErr:       "invalid_grant",
		},
		{
			name: "client_id mismatch: 400 invalid_grant",
			mutate: func(f url.Values) url.Values {
				f.Set("client_id", otherClient)
				return f
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name: "redirect_uri mismatch: 400 invalid_grant",
			mutate: func(f url.Values) url.Values {
				f.Set("redirect_uri", "http://127.0.0.1:9999/other")
				return f
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
		{
			name: "bad verifier: 400 invalid_grant",
			mutate: func(f url.Values) url.Values {
				f.Set("code_verifier", "this-is-not-the-right-verifier-at-all")
				return f
			},
			wantCode: http.StatusBadRequest,
			wantErr:  "invalid_grant",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := tc.mutate(newCode(t))

			if tc.exchangeTwice {
				first := doTokenPost(t, s, form)
				require.Equal(t, http.StatusOK, first.Code, "first exchange of a fresh code must succeed before replay is attempted")
			}

			rec := doTokenPost(t, s, form)
			assert.Equal(t, tc.wantCode, rec.Code)

			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

			if tc.wantErr != "" {
				assert.Equal(t, tc.wantErr, body["error"])
				return
			}

			require.Equal(t, http.StatusOK, tc.wantCode)
			assert.True(t, strings.HasPrefix(body["access_token"].(string), accesstoken.TokenPrefix))
			assert.Equal(t, "Bearer", body["token_type"])
			expiresIn, ok := body["expires_in"].(float64)
			require.True(t, ok, "expires_in must be numeric")
			assert.Greater(t, expiresIn, float64(0))
			assert.Equal(t, "role:read classes:default/demo-agent", body["scope"])

			require.NotEmpty(t, minter.calls)
			last := minter.calls[len(minter.calls)-1]
			assert.Equal(t, accesstoken.RoleRead, last.Role)
			assert.Equal(t, []string{"default/demo-agent"}, last.ScopeClasses)
			assert.False(t, last.Unfiltered)
			assert.Equal(t, subject, last.Owner)
			assert.Equal(t, "demo tool", last.ClientName)
			assert.Equal(t, validClient, last.ClientID)
		})
	}
}

// TestTokenEndpointMinterNilIsUnavailable asserts the Minter==nil fail-closed
// path: the route still mounts (gated on Consent, not Minter), but every
// exchange 503s rather than panicking.
func TestTokenEndpointMinterNilIsUnavailable(t *testing.T) {
	s := newTestServerWithMinter(t, nil)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	p, err := oauth.NewPKCE()
	require.NoError(t, err)
	code, err := s.authCodes.NewCode(authCodeEntry{
		ClientID:    validClient,
		RedirectURI: "http://127.0.0.1:7777/cb",
		Challenge:   p.Challenge,
		Subject:     identity.CanonicalFromTrusted("alice", "test fixture"),
		Role:        accesstoken.RoleRead,
		Unfiltered:  true,
	})
	require.NoError(t, err)

	rec := doTokenPost(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:7777/cb"},
		"client_id":     {validClient},
		"code_verifier": {p.Verifier},
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "server_error", body["error"])
}

// TestTokenEndpointMintFailureIs500 asserts a Minter error surfaces as 500
// server_error, logged, never a 2xx with a partial body.
func TestTokenEndpointMintFailureIs500(t *testing.T) {
	minter := &fakeMinter{err: assertAnError{}}
	s := newTestServerWithMinter(t, minter)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	p, err := oauth.NewPKCE()
	require.NoError(t, err)
	code, err := s.authCodes.NewCode(authCodeEntry{
		ClientID:    validClient,
		RedirectURI: "http://127.0.0.1:7777/cb",
		Challenge:   p.Challenge,
		Subject:     identity.CanonicalFromTrusted("alice", "test fixture"),
		Role:        accesstoken.RoleRead,
		Unfiltered:  true,
	})
	require.NoError(t, err)

	rec := doTokenPost(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://127.0.0.1:7777/cb"},
		"client_id":     {validClient},
		"code_verifier": {p.Verifier},
	})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "server_error", body["error"])
}

// assertAnError is a trivial non-nil error for TestTokenEndpointMintFailureIs500.
type assertAnError struct{}

func (assertAnError) Error() string { return "mint boom" }
