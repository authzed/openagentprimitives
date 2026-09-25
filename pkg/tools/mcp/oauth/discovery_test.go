package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildAuthServerMeta returns a valid RFC 8414 response body.
func buildAuthServerMeta(authEP, tokenEP string, scopes []string) []byte {
	m := Metadata{
		AuthorizationEndpoint: authEP,
		TokenEndpoint:         tokenEP,
		ScopesSupported:       scopes,
	}
	b, _ := json.Marshal(m)
	return b
}

// TestDiscover_WWWAuthenticate covers the RFC 9728 path:
//
//	MCP server → 401 + WWW-Authenticate resource_metadata=<URL>
//	→ resource server metadata → auth server base → /.well-known/
func TestDiscover_WWWAuthenticate(t *testing.T) {
	// Stand up the fake auth server metadata endpoint.
	authMeta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildAuthServerMeta(
			"https://auth.example.com/authorize",
			"https://auth.example.com/token",
			[]string{"read", "write"},
		))
	}))
	t.Cleanup(authMeta.Close)

	// The MCP server serves BOTH the 401 and its own resource metadata, which
	// is what RFC 9728 §3.3 requires: the resource_metadata URL belongs to the
	// resource server's own origin. Modelling them as two servers on two ports
	// made the fixture cross-origin — which is precisely the shape an attacker
	// with only header injection produces, since the resource metadata it
	// points at is one it controls. The AS it names is still a THIRD origin,
	// because a cross-origin AS is the normal deployment and is not refused.
	var mcpSrv *httptest.Server
	mcpSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_servers": []string{authMeta.URL},
			})
			return
		}
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="mcp", resource_metadata="`+mcpSrv.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(mcpSrv.Close)

	md, err := Discover(context.Background(), http.DefaultClient, mcpSrv.URL)
	require.NoError(t, err, "Discover")
	assert.Equal(t, "https://auth.example.com/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, "https://auth.example.com/token", md.TokenEndpoint)
	assert.Len(t, md.ScopesSupported, 2, "ScopesSupported")
}

// TestDiscover_DirectFallback covers the fallback: no WWW-Authenticate,
// so we try <mcpURL>/.well-known/oauth-authorization-server directly.
func TestDiscover_DirectFallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// Return 200 (not 401) with no WWW-Authenticate.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildAuthServerMeta(
			"https://direct.example.com/auth",
			"https://direct.example.com/token",
			nil,
		))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	md, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	require.NoError(t, err, "Discover")
	assert.Equal(t, "https://direct.example.com/auth", md.AuthorizationEndpoint)
}

// TestDiscover_BothPathsFail verifies the error names tried URLs and, per the
// finding fix, the per-path failure reason for each attempt.
func TestDiscover_BothPathsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	require.Error(t, err, "Discover must error when both paths fail")
	assert.Contains(t, err.Error(), "/.well-known/oauth-authorization-server",
		"error should name the well-known path that was tried")
	assert.Contains(t, err.Error(), "auth server metadata:",
		"error should name WHY the well-known fetch failed, not just the URL")
}

// TestDiscover_ResourceMetadataFetchFailNamed verifies that when the RFC 9728
// resource_metadata URL fetch itself fails, the discovery error names that
// URL and its failure reason rather than discarding the intermediate error.
func TestDiscover_ResourceMetadataFetchFailNamed(t *testing.T) {
	// MCP server: 401 with a WWW-Authenticate pointing at a resource_metadata
	// URL that returns 500. Both the well-known fallback and the resource
	// metadata fetch should be named in the final error.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/resource-meta":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/.well-known/oauth-authorization-server":
			http.NotFound(w, r)
		default:
			w.Header().Set("WWW-Authenticate",
				`Bearer realm="mcp", resource_metadata="`+srv.URL+`/resource-meta"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := Discover(context.Background(), http.DefaultClient, srv.URL)
	require.Error(t, err, "Discover must error when resource metadata fetch fails")
	assert.Contains(t, err.Error(), "/resource-meta",
		"error should name the resource_metadata URL that was tried")
	assert.Contains(t, err.Error(), "resource metadata: ",
		"error should name WHY the resource metadata fetch failed (status 500)")
}

func TestParseResourceMetadata(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "Bearer with realm + resource_metadata: extracts metadata URL",
			header: `Bearer realm="mcp", resource_metadata="https://example.com/meta"`,
			want:   "https://example.com/meta",
		},
		{
			name:   "Bearer with only resource_metadata: extracts metadata URL",
			header: `Bearer resource_metadata="https://a.b/c"`,
			want:   "https://a.b/c",
		},
		{
			name:   "Bearer with realm only and no metadata: returns empty",
			header: `Bearer realm="x"`,
			want:   "",
		},
		{
			name:   "empty header: returns empty",
			header: ``,
			want:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, parseResourceMetadata(tc.header))
		})
	}
}
