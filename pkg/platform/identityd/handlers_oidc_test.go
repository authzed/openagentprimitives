package identityd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	fakekind "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/fake"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

// newOIDCFixture builds a Server that registers both the real
// fakekind.WebAuthenticator (for state-scripted OIDC tests) and the
// package-local fakeAuthenticator (for empty-canonical + begin-URL tests).
// externalBase controls the Secure cookie flag.
func newOIDCFixture(t *testing.T, externalBase string) *Server {
	t.Helper()
	fx := newLinkFixture(t) // the base fixture wires K8s + signer
	// Override to use the real fakekind authenticator (uses the global
	// SetCanonicalForState registry) alongside the inline fake for
	// empty-canonical testing.
	srv := NewServer(Deps{
		K8s:             fx.c,
		LinkSigner:      fx.signer,
		ExternalBaseURL: func() string { return externalBase },
		Authenticators: map[string]channelkinds.WebAuthenticator{
			"fake":       fakekind.Kind{}.WebAuthenticator(channelkinds.WebAuthDeps{ExternalBaseURL: externalBase}),
			"fake-blank": blankCanonicalAuth{},
		},
	})
	return srv
}

// blankCanonicalAuth is a test-only authenticator whose Complete always
// returns an empty canonical with no error, exercising the defensive
// empty-canonical branch in handleOIDCCallback.
type blankCanonicalAuth struct{}

func (blankCanonicalAuth) Begin(_ context.Context, state string) (string, error) {
	return "/oidc/callback/fake-blank?state=" + url.QueryEscape(state), nil
}

func (blankCanonicalAuth) Complete(_ context.Context, _ channelkinds.CallbackParams) (string, error) {
	return "", nil // always returns empty canonical, no error
}

// primeStateStore mints a valid signed deep-link, stores it in the
// server's state store, and returns the state token and the raw link.
// Both values are needed: the token goes in the callback URL, the raw
// link is asserted against the redirect destination.
// primeStateStoreFor is primeStateStore for a callback under a DIFFERENT
// authenticator than "fake". A state is bound to the sign-in path that minted
// it, so a fixture aimed at another callback must mint for that one.
func primeStateStoreFor(t *testing.T, srv *Server, subject identity.Subject, authenticator string) (stateTok, linkRaw string) {
	t.Helper()
	var err error
	linkRaw, err = srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef: "default/s1",
		Subject:    subject,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	stateTok, err = srv.stateStore.NewState(linkRaw, testLoginBinding, authenticator)
	require.NoError(t, err)
	return stateTok, linkRaw
}

func primeStateStore(t *testing.T, srv *Server, subject identity.Subject) (stateTok, linkRaw string) {
	t.Helper()
	var err error
	linkRaw, err = srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef: "default/s1",
		Subject:    subject,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	stateTok, err = srv.stateStore.NewState(linkRaw, testLoginBinding, "fake")
	require.NoError(t, err)
	return stateTok, linkRaw
}

// testLoginBinding is the per-flow browser binding these fixtures pretend the
// begin handler minted. doOIDCCallback sends it as loginBindingCookie, so every
// callback test below walks the flow as the browser that STARTED it — which is
// the only browser allowed to finish it (see login_binding.go).
const testLoginBinding = "test-login-binding"

// doOIDCCallback fires a GET /oidc/callback/<kind>?code=<code>&state=<state>
// at the server and returns the recorder.
func doOIDCCallback(t *testing.T, srv *Server, kind, code, state string) *httptest.ResponseRecorder {
	t.Helper()
	return doOIDCCallbackWithBinding(t, srv, kind, code, state, testLoginBinding)
}

// doOIDCCallbackNoBinding is doOIDCCallback for a browser that did NOT start
// this login — no binding cookie at all. That is the login-CSRF shape: the
// attacker can hand over (code, state) but not a cookie.
func doOIDCCallbackNoBinding(t *testing.T, srv *Server, kind, code, state string) *httptest.ResponseRecorder {
	t.Helper()
	return doOIDCCallbackWithBinding(t, srv, kind, code, state, "")
}

// doOIDCCallbackWithBinding is the shared builder. A test that drove the REAL
// begin handler must pass the binding off THAT response, not testLoginBinding —
// the handler mints a random one, which is the whole point.
func doOIDCCallbackWithBinding(t *testing.T, srv *Server, kind, code, state, binding string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/oidc/callback/" + kind
	q := url.Values{}
	if code != "" {
		q.Set("code", code)
	}
	if state != "" {
		q.Set("state", state)
	}
	rawURL := path
	if len(q) > 0 {
		rawURL += "?" + q.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, rawURL, nil)
	if binding != "" {
		req.AddCookie(&http.Cookie{Name: loginBindingCookie, Value: binding})
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// findCookie returns the named cookie from the response, or nil.
func findCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestHandleOIDCCallback covers the happy path and all documented error
// branches of handleOIDCCallback in a single table-driven test. Cases
// share the same server setup shape; they differ in inputs (kind, state
// token, scripted canonical) and expected outcome (status + body/cookie).
func TestHandleOIDCCallback(t *testing.T) {
	const canonical = "user:alice@example.com"

	cases := []struct {
		name string
		// pre is called before the request to prepare state.
		pre func(t *testing.T, srv *Server) (kind, code, state string)
		// check asserts the outcome.
		check func(t *testing.T, rec *httptest.ResponseRecorder, linkRaw string)
	}{
		{
			name: "happy path: valid state + scripted canonical → 302 to /link + cookie set",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				stateTok, _ := primeStateStore(t, srv, canonical)
				fakekind.SetCanonicalForState(stateTok, canonical)
				t.Cleanup(func() { fakekind.ResetCanonicalForState(stateTok) })
				return "fake", "c123", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				require.Equal(t, http.StatusFound, rec.Code)
				loc := rec.Header().Get("Location")
				assert.True(t, strings.HasPrefix(loc, "/link?"), "Location must point to /link")
				assert.Contains(t, loc, "d=", "Location must carry d=")
				assert.Contains(t, loc, "sig=", "Location must carry sig=")

				c := findCookie(rec, cookieName)
				require.NotNil(t, c, "idd_session cookie must be set")
				assert.True(t, c.HttpOnly, "cookie must be HttpOnly")
				assert.Equal(t, http.SameSiteLaxMode, c.SameSite, "cookie must be SameSite=Lax")
				assert.Greater(t, c.MaxAge, 0, "cookie must have a positive MaxAge")
			},
		},
		{
			name: "unknown kind → 404 Sign-in unavailable",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				return "nonexistent", "c", "s"
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusNotFound, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
			},
		},
		{
			name: "missing state param → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				return "fake", "c", "" // empty state
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
		{
			name: "state already consumed: second call → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				stateTok, _ := primeStateStore(t, srv, canonical)
				fakekind.SetCanonicalForState(stateTok, canonical)
				t.Cleanup(func() { fakekind.ResetCanonicalForState(stateTok) })
				// First call consumes the token.
				rec := doOIDCCallback(t, srv, "fake", "c", stateTok)
				require.Equal(t, http.StatusFound, rec.Code, "first call must succeed")
				// Return the same token for the second call.
				return "fake", "c", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
		{
			name: "state expired (ttl=0) → 400 Sign-in session expired",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				srv.stateStore.ttl = 0 // immediate expiry
				stateTok, _ := primeStateStore(t, srv, canonical)
				// No need to script a canonical — the store will reject on Consume.
				return "fake", "c", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusBadRequest, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in session expired")
			},
		},
		{
			name: "authenticator.Complete returns ErrAuthenticatorUnavailable → 500 Sign-in unavailable",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				stateTok, _ := primeStateStore(t, srv, canonical)
				// Do NOT script a canonical → fake returns ErrAuthenticatorUnavailable.
				return "fake", "c", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in unavailable")
			},
		},
		{
			name: "authenticator returns empty canonical → 500 Sign-in failed",
			pre: func(t *testing.T, srv *Server) (string, string, string) {
				t.Helper()
				// Minted FOR fake-blank: a state is bound to the sign-in path that
				// minted it, so one minted for "fake" is refused here.
				stateTok, _ := primeStateStoreFor(t, srv, canonical, "fake-blank")
				// The "fake-blank" kind's Complete always returns "", nil.
				return "fake-blank", "c", stateTok
			},
			check: func(t *testing.T, rec *httptest.ResponseRecorder, _ string) {
				t.Helper()
				assert.Equal(t, http.StatusInternalServerError, rec.Code)
				assert.Contains(t, rec.Body.String(), "Sign-in failed")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOIDCFixture(t, "https://identityd.example.org")
			kind, code, state := tc.pre(t, srv)
			rec := doOIDCCallback(t, srv, kind, code, state)
			tc.check(t, rec, "")
		})
	}
}

// TestHandleOIDCCallback_CookieSecureFlag exercises the Secure attribute
// independently for https and http external URLs, since the table above
// always uses https.
func TestHandleOIDCCallback_CookieSecureFlag(t *testing.T) {
	const canonical = "user:alice@example.com"

	cases := []struct {
		name         string
		externalBase string
		wantSecure   bool
	}{
		{
			name:         "https external URL → Secure=true",
			externalBase: "https://identityd.example.org",
			wantSecure:   true,
		},
		{
			name:         "http external URL → Secure=false",
			externalBase: "http://localhost:8080",
			wantSecure:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOIDCFixture(t, tc.externalBase)

			stateTok, _ := primeStateStore(t, srv, canonical)
			fakekind.SetCanonicalForState(stateTok, canonical)
			t.Cleanup(func() { fakekind.ResetCanonicalForState(stateTok) })

			rec := doOIDCCallback(t, srv, "fake", "c", stateTok)
			require.Equal(t, http.StatusFound, rec.Code)

			c := findCookie(rec, cookieName)
			require.NotNil(t, c, "idd_session cookie must be set")
			assert.Equal(t, tc.wantSecure, c.Secure, "Secure flag mismatch for %s", tc.externalBase)
		})
	}
}

// TestHandleOIDCCallback_RedirectPreservesDeepLink verifies that the
// Location header's d= and sig= values reconstruct the original linkRaw
// when joined with a dot, so that /link can re-verify the HMAC signature.
func TestHandleOIDCCallback_RedirectPreservesDeepLink(t *testing.T) {
	const canonical = "user:alice@example.com"

	srv := newOIDCFixture(t, "https://identityd.example.org")
	stateTok, linkRaw := primeStateStore(t, srv, canonical)
	fakekind.SetCanonicalForState(stateTok, canonical)
	t.Cleanup(func() { fakekind.ResetCanonicalForState(stateTok) })

	rec := doOIDCCallback(t, srv, "fake", "c", stateTok)
	require.Equal(t, http.StatusFound, rec.Code)

	loc := rec.Header().Get("Location")
	parsed, err := url.Parse(loc)
	require.NoError(t, err, "Location must be a valid URL")

	q := parsed.Query()
	d := q.Get("d")
	sig := q.Get("sig")
	assert.NotEmpty(t, d, "d= must be present in redirect")
	assert.NotEmpty(t, sig, "sig= must be present in redirect")

	// Rejoin and verify the reconstructed linkRaw matches the original.
	reconstructed := d + "." + sig
	assert.Equal(t, linkRaw, reconstructed,
		"reconstructed linkRaw from redirect must match the original stored link")
}

// TestHandleOIDCCallback_NextRedirect verifies that when the consumed state
// entry carries a `next` destination (set by /oidc/login's
// NewStateWithNext), the callback redirects there after setting the cookie,
// instead of reconstructing the legacy /link URL.
// next must be a safe relative path — absolute URLs are now rejected by
// redirectNextOrLink to prevent open redirects.
func TestHandleOIDCCallback_NextRedirect(t *testing.T) {
	const (
		canonical = "user:alice@example.com"
		nextDest  = "/artifacts/v/abc" // safe relative path
	)

	srv := newOIDCFixture(t, "https://identityd.example.org")

	linkRaw, err := srv.deps.LinkSigner.Mint(passthroughlink.Payload{
		SessionRef: "default/s1",
		Subject:    canonical,
		ExpiresAt:  time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	stateTok, err := srv.stateStore.NewStateWithNext(linkRaw, nextDest, testLoginBinding, "fake")
	require.NoError(t, err)

	fakekind.SetCanonicalForState(stateTok, canonical)
	t.Cleanup(func() { fakekind.ResetCanonicalForState(stateTok) })

	rec := doOIDCCallback(t, srv, "fake", "c", stateTok)
	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, nextDest, rec.Header().Get("Location"),
		"callback must redirect to the state's next, not /link")

	// Cookie must still be set on the next-redirect path.
	c := findCookie(rec, cookieName)
	require.NotNil(t, c, "idd_session cookie must be set even when redirecting to next")
}

// TestHandleOIDCCallback_MethodNotAllowed ensures the GET guard rejects
// non-GET requests with a 405.
func TestHandleOIDCCallback_MethodNotAllowed(t *testing.T) {
	srv := newOIDCFixture(t, "https://identityd.example.org")
	req := httptest.NewRequest(http.MethodPost, "/oidc/callback/fake", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, http.MethodGet, rec.Header().Get("Allow"))
}
