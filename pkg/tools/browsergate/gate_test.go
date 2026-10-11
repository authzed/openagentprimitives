package browsergate

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/browserpolicy"
)

const testToken = "test-secret-token"

// quietLogger returns a slog logger that discards everything, so a handler's
// best-effort error logging cannot pollute test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writePolicy writes doc as JSON to a fresh file under t.TempDir and returns
// the path.
func writePolicy(t *testing.T, mode string, sites ...browserpolicy.Site) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	rewritePolicy(t, path, mode, sites...)
	return path
}

// rewritePolicy overwrites path with a new PolicyDoc and bumps its mtime a
// second into the future so an mtime-based reloader sees the change even on a
// coarse-resolution filesystem.
func rewritePolicy(t *testing.T, path, mode string, sites ...browserpolicy.Site) {
	t.Helper()
	doc := `{"mode":"` + mode + `","sites":[`
	parts := make([]string, 0, len(sites))
	for _, s := range sites {
		p := `{"host":"` + s.Host + `"`
		if s.Exact {
			p += `,"exact":true`
		}
		p += `}`
		parts = append(parts, p)
	}
	doc += strings.Join(parts, ",") + `]}`
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))
}

// newTestGate builds a Gate for the allowlist/blocklist under test, pointed at
// devtoolsURL, with a quiet logger. The dialContext is NOT overridden here;
// individual tests inject one when they need to reach a backend.
func newTestGate(t *testing.T, devtoolsURL, policyPath string) *Gate {
	t.Helper()
	g, err := New(Config{
		ProxyAddr:   "127.0.0.1:0",
		GateAddr:    "127.0.0.1:0",
		DevtoolsURL: devtoolsURL,
		Token:       testToken,
		PolicyPath:  policyPath,
	})
	require.NoError(t, err)
	g.logger = quietLogger()
	g.policy.logger = quietLogger()
	return g
}

// bufConn adapts a net.Conn whose leading bytes have already been buffered into
// a bufio.Reader, so a tls.Client built on top reads through the buffer first.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// sendCONNECT dials proxyAddr, issues a CONNECT for target, and returns the
// raw conn, the parsed response, and the buffered reader positioned just past
// the response headers (for tunnelling after a 200).
func sendCONNECT(t *testing.T, proxyAddr, target string) (net.Conn, *http.Response, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	require.NoError(t, err)
	_, err = conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"))
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	require.NoError(t, err)
	return conn, resp, br
}

func TestCONNECT(t *testing.T) {
	// A TLS backend the allowed tunnel reaches; it answers any path with "pong".
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pong"))
	}))
	t.Cleanup(backend.Close)
	backendHostPort := strings.TrimPrefix(backend.URL, "https://")

	cases := []struct {
		name       string
		mode       string
		sites      []browserpolicy.Site
		target     string
		inject     bool // map every dial to the backend
		wantStatus int
		wantBody   string // substring, checked only for deny cases
		tunnel     bool   // complete a TLS exchange over the established tunnel
	}{
		{
			name: "allowlist admits listed host: tunnel established",
			mode: browserpolicy.ModeAllowlist, sites: []browserpolicy.Site{{Host: "allowed.test"}},
			target: "allowed.test:443", inject: true, wantStatus: 200, tunnel: true,
		},
		{
			name: "allowlist denies unlisted host: 403 not in allowlist",
			mode: browserpolicy.ModeAllowlist, sites: []browserpolicy.Site{{Host: "allowed.test"}},
			target: "other.test:443", wantStatus: 403, wantBody: "(rule: not in allowlist)",
		},
		{
			name: "port not in {80,443} denied: 403",
			mode: browserpolicy.ModeAllowlist, sites: []browserpolicy.Site{{Host: "allowed.test"}},
			target: "allowed.test:8443", wantStatus: 403,
		},
		{
			name: "blocklist denies listed host: 403 explicit denylist",
			mode: browserpolicy.ModeBlocklist, sites: []browserpolicy.Site{{Host: "blocked.test"}},
			target: "blocked.test:443", wantStatus: 403, wantBody: "(rule: explicit denylist)",
		},
		{
			name: "blocklist admits unlisted host: tunnel established",
			mode: browserpolicy.ModeBlocklist, sites: []browserpolicy.Site{{Host: "blocked.test"}},
			target: "fine.test:443", inject: true, wantStatus: 200,
		},
		{
			name: "private IP literal denied: 403 private or metadata address",
			mode: browserpolicy.ModeAllowlist, sites: []browserpolicy.Site{{Host: "allowed.test"}},
			target: "10.0.0.5:443", wantStatus: 403, wantBody: "(rule: private or metadata address)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newTestGate(t, backend.URL, writePolicy(t, tc.mode, tc.sites...))
			if tc.inject {
				g.dialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, network, backendHostPort)
				}
			}
			proxy := httptest.NewServer(g.ProxyHandler())
			t.Cleanup(proxy.Close)
			proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

			conn, resp, br := sendCONNECT(t, proxyAddr, tc.target)
			t.Cleanup(func() { _ = conn.Close() })
			assert.Equal(t, tc.wantStatus, resp.StatusCode)

			if tc.wantStatus != 200 {
				body, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				if tc.wantBody != "" {
					assert.Contains(t, string(body), tc.wantBody)
				}
				return
			}

			if tc.tunnel {
				tconn := tls.Client(&bufConn{Conn: conn, r: br}, &tls.Config{InsecureSkipVerify: true})
				require.NoError(t, tconn.SetDeadline(time.Now().Add(2*time.Second)))
				_, err := tconn.Write([]byte("GET /ping HTTP/1.1\r\nHost: allowed.test\r\nConnection: close\r\n\r\n"))
				require.NoError(t, err)
				exResp, err := http.ReadResponse(bufio.NewReader(tconn), &http.Request{Method: http.MethodGet})
				require.NoError(t, err)
				body, err := io.ReadAll(exResp.Body)
				require.NoError(t, err)
				assert.Equal(t, "pong", string(body))
			}
		})
	}
}

// TestCONNECTDialTimeResolvedLoopback proves the dial-time control hook rejects
// a hostname that is policy-allowed but resolves to a loopback address — the
// resolved-IP guard, not the name guard. "localhost" is admitted by the
// allowlist (it is not an IP literal, so Decide lets it through) but resolves
// to 127.0.0.1/::1, which the Dialer.Control hook refuses.
func TestCONNECTDialTimeResolvedLoopback(t *testing.T) {
	g := newTestGate(t, DefaultDevtoolsURL, writePolicy(t, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "localhost"}))
	// No dialContext injection: the real guarded dialer must fire.
	proxy := httptest.NewServer(g.ProxyHandler())
	t.Cleanup(proxy.Close)
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	conn, resp, _ := sendCONNECT(t, proxyAddr, "localhost:443")
	t.Cleanup(func() { _ = conn.Close() })
	assert.Equal(t, 403, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "(rule: private or metadata address)")
}

// TestProxyRefusesLoopbackAndInternalPorts proves Chrome cannot use the proxy
// to reach the gate's own DevTools surface (or any loopback service) and thus
// bypass the token. Even in blocklist mode — which admits every host by NAME —
// a loopback IP target is refused by the private-address rule, and an internal
// port is refused by the port rule. Both deny before any dial.
func TestProxyRefusesLoopbackAndInternalPorts(t *testing.T) {
	// Blocklist with no entries: every host is admitted by name, so only the
	// IP-literal and port guards can produce these denials.
	g := newTestGate(t, DefaultDevtoolsURL, writePolicy(t, browserpolicy.ModeBlocklist))
	proxy := httptest.NewServer(g.ProxyHandler())
	t.Cleanup(proxy.Close)
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	cases := []struct {
		name, target, wantBody string
	}{
		{name: "loopback on an open port is refused", target: "127.0.0.1:443", wantBody: "(rule: private or metadata address)"},
		{name: "devtools port is refused by the port rule", target: "127.0.0.1:9222", wantBody: "port 9222 is not allowed"},
		{name: "gate port is refused by the port rule", target: "127.0.0.1:9223", wantBody: "port 9223 is not allowed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, resp, _ := sendCONNECT(t, proxyAddr, tc.target)
			t.Cleanup(func() { _ = conn.Close() })
			require.Equal(t, 403, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Contains(t, string(body), tc.wantBody)
		})
	}
}

func TestPlainHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(backend.Close)
	backendHostPort := strings.TrimPrefix(backend.URL, "http://")

	g := newTestGate(t, DefaultDevtoolsURL, writePolicy(t, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "allowed.test"}))
	g.dialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, backendHostPort)
	}
	proxy := httptest.NewServer(g.ProxyHandler())
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	require.NoError(t, err)

	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 3 * time.Second}

	t.Run("allowed host is forwarded", func(t *testing.T) {
		resp, err := client.Get("http://allowed.test/x")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "ok", string(body))
	})

	t.Run("denied host returns 403", func(t *testing.T) {
		resp, err := client.Get("http://other.test/x")
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, 403, resp.StatusCode)
		assert.Contains(t, string(body), "(rule: not in allowlist)")
	})
}

// TestPolicyReload proves an mtime change re-reads the file, flipping a denied
// host to allowed. Exercised through the decision path (which runs the
// stat-and-reload) rather than a listener, to isolate the reload logic.
func TestPolicyReload(t *testing.T) {
	path := writePolicy(t, browserpolicy.ModeAllowlist) // empty allowlist: denies everything
	g := newTestGate(t, DefaultDevtoolsURL, path)

	dec := g.decide("allowed.test")
	assert.False(t, dec.Allowed, "v1 empty allowlist must deny")

	rewritePolicy(t, path, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "allowed.test"})

	dec = g.decide("allowed.test")
	assert.True(t, dec.Allowed, "v2 allowlist must admit after mtime-triggered reload")
}

// TestPolicyReloadKeepsOldOnParseFailure proves a corrupt rewrite is ignored:
// the last good policy stays in force, never failing open.
func TestPolicyReloadKeepsOldOnParseFailure(t *testing.T) {
	path := writePolicy(t, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "allowed.test"})
	g := newTestGate(t, DefaultDevtoolsURL, path)
	require.True(t, g.decide("allowed.test").Allowed)

	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	future := time.Now().Add(time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	assert.True(t, g.decide("allowed.test").Allowed, "corrupt reload must keep the old policy")
}

func TestNewFailsClosed(t *testing.T) {
	goodPolicy := writePolicy(t, browserpolicy.ModeAllowlist, browserpolicy.Site{Host: "a.test"})

	t.Run("empty token errors", func(t *testing.T) {
		_, err := New(Config{DevtoolsURL: DefaultDevtoolsURL, PolicyPath: goodPolicy})
		require.Error(t, err)
	})
	t.Run("missing policy file errors", func(t *testing.T) {
		_, err := New(Config{Token: testToken, DevtoolsURL: DefaultDevtoolsURL, PolicyPath: filepath.Join(t.TempDir(), "nope.json")})
		require.Error(t, err)
	})
	t.Run("unknown mode errors", func(t *testing.T) {
		p := writePolicy(t, "sideways")
		_, err := New(Config{Token: testToken, DevtoolsURL: DefaultDevtoolsURL, PolicyPath: p})
		require.Error(t, err)
	})
	t.Run("good config succeeds", func(t *testing.T) {
		_, err := New(Config{Token: testToken, DevtoolsURL: DefaultDevtoolsURL, PolicyPath: goodPolicy})
		require.NoError(t, err)
	})
}
