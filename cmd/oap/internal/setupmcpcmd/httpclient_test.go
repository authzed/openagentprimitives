package setupmcpcmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
	"github.com/authzed/openagentprimitives/pkg/x/safehttp"
)

// newLoopbackMCPServer stands in for a local install's webd: an MCP endpoint
// that challenges with its protected-resource metadata, plus the OAuth
// authorization-server metadata and a registration endpoint, all on one
// loopback origin. registrationEndpoint overrides where the metadata says to
// register ("" keeps it on this server); redirectTo, when set, makes /hop
// redirect there.
func newLoopbackMCPServer(t *testing.T, registrationEndpoint, redirectTo string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		assert.NoError(t, json.NewEncoder(w).Encode(v))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+srv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"resource": srv.URL + "/mcp", "authorization_servers": []string{srv.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		reg := registrationEndpoint
		if reg == "" {
			reg = srv.URL + "/oauth/register"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"issuer":                           srv.URL,
			"authorization_endpoint":           srv.URL + "/oauth/authorize",
			"token_endpoint":                   srv.URL + "/oauth/token",
			"registration_endpoint":            reg,
			"response_types_supported":         []string{"code"},
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("/oauth/register", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"client_id": "demo-client-id"})
	})
	mux.HandleFunc("/hop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTo, http.StatusFound)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newOtherLoopbackService is some unrelated service on the caller's machine —
// the thing an SSRF would be aimed at. It records whether it was ever reached.
func newOtherLoopbackService(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	reached := new(bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"client_id":"stolen"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, reached
}

// TestMintClientReachesNamedLoopbackServer is the local/desktop install case:
// the server URL is loopback, and discovery plus registration against it must
// work. The guarded default client is asserted to FAIL the same discovery, so
// this test keeps explaining why the command needs its own client.
func TestMintClientReachesNamedLoopbackServer(t *testing.T) {
	ctx := context.Background()
	srv := newLoopbackMCPServer(t, "", "")

	_, err := oauth.Discover(ctx, safehttp.Client(), mcpURL(srv.URL))
	require.Error(t, err, "the guarded default client refuses a loopback server; if this starts passing, mintHTTPClient is no longer needed")

	hc, err := mintHTTPClient(srv.URL)
	require.NoError(t, err)

	meta, err := oauth.Discover(ctx, hc, mcpURL(srv.URL))
	require.NoError(t, err, "discovery against the named loopback server must succeed")
	assert.Equal(t, srv.URL+"/oauth/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/oauth/token", meta.TokenEndpoint)
	require.Equal(t, srv.URL+"/oauth/register", meta.RegistrationEndpoint)

	clientID, _, err := oauth.Register(ctx, hc, meta.RegistrationEndpoint, []string{"http://127.0.0.1:7777/callback"})
	require.NoError(t, err, "registration against the named loopback server must succeed")
	assert.Equal(t, "demo-client-id", clientID)
}

// TestMintClientGuardsEveryOtherOrigin: the exemption is the named origin and
// nothing else. Anything the server's documents or redirects point at is
// still refused when it is loopback or private.
func TestMintClientGuardsEveryOtherOrigin(t *testing.T) {
	ctx := context.Background()

	t.Run("a registration endpoint on another loopback port: refused, never reached", func(t *testing.T) {
		other, reached := newOtherLoopbackService(t)
		srv := newLoopbackMCPServer(t, other.URL+"/register", "")
		hc, err := mintHTTPClient(srv.URL)
		require.NoError(t, err)

		_, _, err = oauth.Register(ctx, hc, other.URL+"/register", []string{"http://127.0.0.1:7777/callback"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked")
		assert.False(t, *reached, "the other loopback service must never see the request")
	})

	t.Run("a redirect from the named server to another loopback port: refused, never reached", func(t *testing.T) {
		other, reached := newOtherLoopbackService(t)
		srv := newLoopbackMCPServer(t, "", other.URL+"/internal")
		hc, err := mintHTTPClient(srv.URL)
		require.NoError(t, err)

		resp, err := hc.Get(srv.URL + "/hop")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked")
		assert.False(t, *reached, "the redirect target must never see the request")
	})

	t.Run("a public named server exempts no loopback address", func(t *testing.T) {
		other, reached := newOtherLoopbackService(t)
		hc, err := mintHTTPClient("https://cluster.example.com")
		require.NoError(t, err)

		resp, err := hc.Get(other.URL + "/internal")
		if resp != nil {
			_ = resp.Body.Close()
		}
		require.Error(t, err)
		assert.Contains(t, err.Error(), "blocked")
		assert.False(t, *reached)
	})
}

func TestMintHTTPClientRejectsUnusableServerURL(t *testing.T) {
	for _, raw := range []string{"", "cluster.example.com", "ftp://cluster.example.com", "https:///mcp", "://bad"} {
		t.Run("server URL "+raw+": error", func(t *testing.T) {
			_, err := mintHTTPClient(raw)
			require.Error(t, err)
		})
	}
}

func TestOriginOf(t *testing.T) {
	cases := []struct {
		name, a, b string
		same       bool
	}{
		{name: "default https port is implied: same", a: "https://cluster.example.com", b: "https://cluster.example.com:443/mcp", same: true},
		{name: "default http port is implied: same", a: "http://localhost", b: "http://localhost:80/x", same: true},
		{name: "host and scheme case are ignored: same", a: "HTTPS://Cluster.Example.com", b: "https://cluster.example.com", same: true},
		{name: "path and query are not part of the origin: same", a: "http://127.0.0.1:8080/mcp", b: "http://127.0.0.1:8080/oauth/token?x=1", same: true},
		{name: "another port on the same host: different", a: "http://127.0.0.1:8080", b: "http://127.0.0.1:8081"},
		{name: "another scheme on the same host and port: different", a: "http://localhost:8443", b: "https://localhost:8443"},
		{name: "localhost and 127.0.0.1 are distinct names: different", a: "http://localhost:8080", b: "http://127.0.0.1:8080"},
		{name: "a host that merely starts with the named one: different", a: "https://cluster.example.com", b: "https://cluster.example.com.attacker.example"},
		{name: "userinfo naming the trusted host: different", a: "https://cluster.example.com", b: "https://cluster.example.com@attacker.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ua, err := url.Parse(tc.a)
			require.NoError(t, err)
			ub, err := url.Parse(tc.b)
			require.NoError(t, err)
			assert.Equal(t, tc.same, originOf(ua) == originOf(ub))
		})
	}
}
