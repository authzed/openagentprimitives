package settingsui

import (
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/cmd/oap/internal/kube"
)

func startTestServer(t *testing.T, deps Deps) *Server {
	t.Helper()
	if deps.SupportDir == "" {
		deps.SupportDir = t.TempDir()
	}
	if deps.State == nil {
		deps.State = func() State { return State{Running: false, Phase: "stopped"} }
	}
	if deps.Logf == nil {
		deps.Logf = t.Logf
	}
	if deps.Clients == nil {
		// New requires Clients (cluster routes call it unguarded). Tests that
		// don't exercise a cluster route get a stub that errors as if the
		// cluster were down — the same "cluster not running" state those routes
		// already handle — rather than a nil seam that would fail construction.
		deps.Clients = func() (*kube.Bundle, error) { return nil, errors.New("settingsui test: cluster not wired") }
	}
	s, err := New(deps)
	require.NoError(t, err)
	require.NoError(t, s.Start(0))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newTestJar returns an empty cookie jar for use by an *http.Client in
// tests. publicsuffix handling is unneeded here — every request in these
// tests targets a single loopback host.
func newTestJar(t *testing.T) http.CookieJar {
	t.Helper()
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	return jar
}

// authedClient exchanges the launch token for the session cookie and returns
// a client that carries it. CheckRedirect stops AT the 302 (rather than
// following it) via http.ErrUseLastResponse — the Jar still records the
// Set-Cookie from that response (cookie processing happens before
// CheckRedirect is consulted), so the returned client is fully authed for
// its next request without this helper needing to inspect whatever "/"
// happens to render.
func authedClient(t *testing.T, s *Server) *http.Client {
	t.Helper()
	jar := newTestJar(t)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(s.URL())
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode, "auth exchange must 302 to /")
	return c
}

func TestAuth(t *testing.T) {
	s := startTestServer(t, Deps{})
	base := "http://" + s.Addr()

	t.Run("no cookie: every route 401s", func(t *testing.T) {
		for _, path := range []string{"/", "/api/state", "/api/config"} {
			resp, err := http.Get(base + path)
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, path)
		}
	})

	t.Run("bad token: 401, no cookie set", func(t *testing.T) {
		resp, err := http.Get(base + "/auth?token=deadbeef")
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		assert.Empty(t, resp.Cookies())
	})

	t.Run("valid token: cookie set HttpOnly+SameSite=Strict, redirect to /", func(t *testing.T) {
		u, _ := url.Parse(s.URL())
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(u.String())
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusFound, resp.StatusCode)
		assert.Equal(t, "/", resp.Header.Get("Location"))
		require.Len(t, resp.Cookies(), 1)
		ck := resp.Cookies()[0]
		assert.True(t, ck.HttpOnly)
		assert.Equal(t, http.SameSiteStrictMode, ck.SameSite)
	})

	t.Run("cookied client reaches /api/state", func(t *testing.T) {
		c := authedClient(t, s)
		resp, err := c.Get(base + "/api/state")
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		b, _ := io.ReadAll(resp.Body)
		assert.Contains(t, string(b), `"phase":"stopped"`)
	})

	t.Run("foreign Origin rejected even with cookie", func(t *testing.T) {
		c := authedClient(t, s)
		req, _ := http.NewRequest(http.MethodGet, base+"/api/state", nil)
		req.Header.Set("Origin", "https://evil.example")
		resp, err := c.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}
