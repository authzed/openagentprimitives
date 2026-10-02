package identityd

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/web/webui"
)

// testAuthorizeSubject is the canonical subject the test helpers inject into
// the request context, standing in for the cookie subject the webui
// framework would normally set via its auth middleware (see
// webui.WithSubjectForTest's doc comment).
var testAuthorizeSubject = "user:" + base64.RawURLEncoding.EncodeToString([]byte("alice@example.org"))

// mintTestClientID registers a DCR client directly through the server's own
// minter (bypassing the HTTP /oauth/register round trip, which Task 6's own
// tests already cover) and returns the resulting client_id.
func mintTestClientID(t *testing.T, s *Server, name string, redirectURIs []string) string {
	t.Helper()
	id, err := s.mintClientID(name, redirectURIs)
	require.NoError(t, err)
	return id
}

// doAuthorizeGet sends GET /oauth/authorize<query> with an authenticated
// subject injected into the request context, the way sibling webui packages
// test AuthLoginIfNecessary/AuthAuthenticated handlers outside the framework
// (webui.WithSubjectForTest).
func doAuthorizeGet(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := webui.WithSubjectForTest(context.Background(), testAuthorizeSubject)
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize"+query, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// doConsentPost sends POST /oauth/consent with form, the same injected
// subject as doAuthorizeGet, and an Origin header matching the test server's
// ExternalBaseURL (the CSRF pin doAuthorizeGet's counterpart must satisfy).
func doConsentPost(t *testing.T, s *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	ctx := webui.WithSubjectForTest(context.Background(), testAuthorizeSubject)
	req := httptest.NewRequest(http.MethodPost, "/oauth/consent", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://example.org")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// extractHiddenValue pulls `value="..."` out of `name="<name>" value="..."`
// in a rendered form body — just enough parsing to recover the pending id
// the consent form carries, without a full HTML parser.
func extractHiddenValue(t *testing.T, body, name string) string {
	t.Helper()
	marker := `name="` + name + `" value="`
	idx := strings.Index(body, marker)
	require.NotEqual(t, -1, idx, "hidden field %q not found in form body", name)
	rest := body[idx+len(marker):]
	end := strings.Index(rest, `"`)
	require.NotEqual(t, -1, end, "unterminated value for hidden field %q", name)
	return rest[:end]
}

// primePendingAuthorize performs the authorize GET for validClient against a
// fixed redirect_uri/state and returns the pending id the consent form
// embeds, for a subsequent POST /oauth/consent.
func primePendingAuthorize(t *testing.T, s *Server, clientID string) string {
	t.Helper()
	query := "?response_type=code&client_id=" + clientID +
		"&redirect_uri=http://127.0.0.1:7777/cb&code_challenge=x&code_challenge_method=S256&state=st"
	rec := doAuthorizeGet(t, s, query)
	require.Equal(t, http.StatusOK, rec.Code, "priming GET must render the consent form")
	return extractHiddenValue(t, rec.Body.String(), "pending")
}

func TestAuthorizeValidation(t *testing.T) {
	s := newTestServerWithConsent(t) // fixture: stub ConsentClasses returns [{ID:"default/demo-agent",DisplayName:"Demo"}]
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	cases := []struct {
		name     string
		query    string
		wantCode int
	}{
		{name: "missing client_id: 400", query: "?response_type=code", wantCode: 400},
		{name: "bogus client_id: 400", query: "?response_type=code&client_id=garbage&redirect_uri=http://127.0.0.1:7777/cb&code_challenge=x&code_challenge_method=S256", wantCode: 400},
		{name: "redirect_uri not registered: 400", query: "?response_type=code&client_id=" + validClient + "&redirect_uri=http://127.0.0.1:9999/other&code_challenge=x&code_challenge_method=S256", wantCode: 400},
		{name: "plain challenge method: 400", query: "?response_type=code&client_id=" + validClient + "&redirect_uri=http://127.0.0.1:7777/cb&code_challenge=x&code_challenge_method=plain", wantCode: 400},
		{name: "happy path renders consent form: 200", query: "?response_type=code&client_id=" + validClient + "&redirect_uri=http://127.0.0.1:7777/cb&code_challenge=x&code_challenge_method=S256&state=st", wantCode: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAuthorizeGet(t, s, tc.query) // helper: injects an authenticated subject into ctx the way sibling tests do
			assert.Equal(t, tc.wantCode, rec.Code)
			if tc.wantCode == 200 {
				body := rec.Body.String()
				assert.Contains(t, body, "demo tool")
				assert.Contains(t, body, `value="read" checked`) // read-only preselected
				assert.Contains(t, body, "default/demo-agent")
				assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), "consent page must refuse framing")
				assert.Equal(t, "frame-ancestors 'none'", rec.Header().Get("Content-Security-Policy"), "consent page must refuse framing")
			}
		})
	}
}

func TestConsentPostMintsCodeAndRedirects(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient) // helper: performs the GET, extracts the hidden pending id from the form

	rec := doConsentPost(t, s, url.Values{
		"pending": {pendingID},
		"role":    {"read"},
		"scope":   {"default/demo-agent"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", loc.Host)
	assert.Equal(t, "st", loc.Query().Get("state"))
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)

	entry, ok := s.authCodes.Consume(code)
	require.True(t, ok)
	assert.Equal(t, "read", entry.Role)
	assert.Equal(t, []string{"default/demo-agent"}, entry.ScopeClasses)
	assert.False(t, entry.Unfiltered)
}

func TestConsentPostDenyNeverMintsCode(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	rec := doConsentPost(t, s, url.Values{
		"pending": {pendingID},
		"deny":    {"1"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:7777", loc.Host)
	assert.Equal(t, "access_denied", loc.Query().Get("error"))
	assert.Equal(t, "st", loc.Query().Get("state"))
	assert.Empty(t, loc.Query().Get("code"), "deny must never mint a code")
}

func TestConsentPostPendingIsSingleUse(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	form := url.Values{"pending": {pendingID}, "role": {"read"}, "scope": {"default/demo-agent"}}
	first := doConsentPost(t, s, form)
	require.Equal(t, http.StatusFound, first.Code)

	second := doConsentPost(t, s, form)
	assert.Equal(t, http.StatusBadRequest, second.Code, "a consumed pending id must be refused on replay")
}

func TestConsentPostRejectsSubjectMismatch(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	otherSubject := "user:" + base64.RawURLEncoding.EncodeToString([]byte("mallory@example.org"))
	ctx := webui.WithSubjectForTest(context.Background(), otherSubject)
	form := url.Values{"pending": {pendingID}, "role": {"read"}, "scope": {"default/demo-agent"}}
	req := httptest.NewRequest(http.MethodPost, "/oauth/consent", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://example.org")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "a different signed-in subject must not redeem another's pending consent")
}

func TestConsentPostRejectsUntrustedOrigin(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	ctx := webui.WithSubjectForTest(context.Background(), testAuthorizeSubject)
	form := url.Values{"pending": {pendingID}, "role": {"read"}, "scope": {"default/demo-agent"}}
	req := httptest.NewRequest(http.MethodPost, "/oauth/consent", strings.NewReader(form.Encode())).WithContext(ctx)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "a mismatched Origin must be refused")
}

func TestConsentPostRejectsUnlistedScope(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	rec := doConsentPost(t, s, url.Values{
		"pending": {pendingID},
		"role":    {"read"},
		"scope":   {"default/not-a-real-class"},
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a scope not in the re-fetched ConsentClasses must be refused")
}

func TestConsentPostEverythingScope(t *testing.T) {
	s := newTestServerWithConsent(t)
	validClient := mintTestClientID(t, s, "demo tool", []string{"http://127.0.0.1:7777/cb"})
	pendingID := primePendingAuthorize(t, s, validClient)

	rec := doConsentPost(t, s, url.Values{
		"pending":   {pendingID},
		"role":      {"full"},
		"scope_all": {"1"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	loc, err := url.Parse(rec.Header().Get("Location"))
	require.NoError(t, err)
	code := loc.Query().Get("code")
	require.NotEmpty(t, code)

	entry, ok := s.authCodes.Consume(code)
	require.True(t, ok)
	assert.True(t, entry.Unfiltered)
	assert.Empty(t, entry.ScopeClasses)
}

// TestOAuthAuthorizationServerRoutesGatedOnConsent is the route-registration
// half of "feature off without SpiceDB": a server built WITHOUT ConsentDeps
// (newTestServer, not newTestServerWithConsent) must 404 the entire
// authorization-server surface, Task 6's metadata/register pair included —
// none of it can complete without a consent-class lookup.
func TestOAuthAuthorizationServerRoutesGatedOnConsent(t *testing.T) {
	s := newTestServer(t)
	require.Nil(t, s.deps.Consent, "fixture must not wire Consent")

	paths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/.well-known/oauth-authorization-server"},
		{http.MethodPost, "/oauth/register"},
		{http.MethodGet, "/oauth/authorize"},
		{http.MethodPost, "/oauth/consent"},
	}
	for _, p := range paths {
		t.Run(p.method+" "+p.path, func(t *testing.T) {
			req := httptest.NewRequest(p.method, p.path, nil)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusNotFound, rec.Code)
		})
	}
}
