package identityd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	mcpoauth "github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// newDiscoveryTestClient builds a fake controller-runtime client pre-loaded
// with the given objects and with the v1alpha1 scheme registered.
func newDiscoveryTestClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, spiceboxv1alpha1.AddToScheme(s))
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// mcpServerWithCred builds a minimal MCPServer with an explicit
// Spec.Auth.Credential (the "named credential" path).
func mcpServerWithCred(ns, name, credName, serverURL string) *spiceboxv1alpha1.MCPServer {
	return &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    name,
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       serverURL,
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Credential: credName,
			},
		},
	}
}

// --- findMCPServerForCredential ---

func TestFindMCPServerForCredential_Match(t *testing.T) {
	srv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", "https://linear.example/mcp")
	c := newDiscoveryTestClient(t, srv)

	got, err := findMCPServerForCredential(context.Background(), c, "linear-oauth")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "linear-mcp", got.Name)
}

func TestFindMCPServerForCredential_NoMatch(t *testing.T) {
	srv := mcpServerWithCred("default", "linear-mcp", "linear-oauth", "https://linear.example/mcp")
	c := newDiscoveryTestClient(t, srv)

	_, err := findMCPServerForCredential(context.Background(), c, "nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestFindMCPServerForCredential_NoNameFallback(t *testing.T) {
	// MCPServer with empty Spec.Auth.Credential — per the no-inference
	// contract, lookup does NOT fall back to metadata.name. Operators
	// must declare spec.auth.credential explicitly.
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "my-creds"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "my-creds",
			Version: "1.0",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://example.com/mcp",
				Transport: "http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Provider: "some-provider",
				// Credential intentionally empty — must NOT match by name.
			},
		},
	}
	c := newDiscoveryTestClient(t, srv)

	_, err := findMCPServerForCredential(context.Background(), c, "my-creds")
	require.Error(t, err, "metadata.name must not serve as a credential-name fallback")
}

// TestFindMCPServerForCredential_ProductionLinearShape pins a
// regression: in the wild, a user-reported "No matching service / No
// service is configured for this credential" error fired on
// /link/oauth/linear-oauth when the cluster carried an MCPServer with:
//
//	metadata.name           = linear-readonly-passthrough
//	spec.auth.type          = oauth
//	spec.auth.provider      = Linear
//	spec.auth.credential    = linear-oauth
//
// (mirrors a real-world passthrough MCPServer shape).
// The lookup function MUST find it by Spec.Auth.Credential even when
// metadata.name differs. If this test fails after a refactor, the
// production OAuth flow breaks.
func TestFindMCPServerForCredential_ProductionLinearShape(t *testing.T) {
	srv := &spiceboxv1alpha1.MCPServer{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "linear-readonly-passthrough"},
		Spec: spiceboxv1alpha1.MCPServerSpec{
			Name:    "linear-readonly-passthrough",
			Version: "1",
			Server: spiceboxv1alpha1.MCPServerServer{
				URL:       "https://mcp.linear.app/mcp",
				Transport: "streamable-http",
			},
			Auth: spiceboxv1alpha1.MCPServerAuth{
				Type:       "oauth",
				Provider:   "Linear",
				Credential: "linear-oauth",
				Header:     "Authorization",
			},
		},
	}
	c := newDiscoveryTestClient(t, srv)

	got, err := findMCPServerForCredential(context.Background(), c, "linear-oauth")
	require.NoError(t, err, "production-shape OAuth Linear MCPServer must be findable by its declared credential")
	require.NotNil(t, got)
	assert.Equal(t, "linear-readonly-passthrough", got.Name,
		"matched MCPServer is the canonical Linear server")
	assert.Equal(t, "oauth", got.Spec.Auth.Type)
	assert.Equal(t, "Linear", got.Spec.Auth.Provider)
}

func TestFindMCPServerForCredential_MultipleMatchesError(t *testing.T) {
	// Two MCPServers in different namespaces both claiming the same
	// credential name — operator misconfiguration.
	srv1 := mcpServerWithCred("ns-a", "linear-a", "linear-oauth", "https://linear.example/mcp")
	srv2 := mcpServerWithCred("ns-b", "linear-b", "linear-oauth", "https://linear.example/mcp")
	c := newDiscoveryTestClient(t, srv1, srv2)

	_, err := findMCPServerForCredential(context.Background(), c, "linear-oauth")
	require.Error(t, err, "multi-match is operator misconfiguration")
	assert.Contains(t, err.Error(), "multiple")
}

// --- discoverOAuthMetadata ---

// buildWellKnownMeta returns a valid RFC 8414 auth-server metadata JSON body
// with the given endpoints. Mirrors the helper in pkg/tools/mcp/oauth/discovery_test.go.
func buildWellKnownMeta(t *testing.T, authEP, tokenEP string) []byte {
	t.Helper()
	b, err := json.Marshal(mcpoauth.Metadata{
		AuthorizationEndpoint: authEP,
		TokenEndpoint:         tokenEP,
	})
	require.NoError(t, err)
	return b
}

func TestDiscoverOAuthMetadata_HappyPath(t *testing.T) {
	// Serve the direct fallback path: GET <base>/.well-known/oauth-authorization-server.
	// pkg/tools/mcp/oauth.Discover probes the MCP root first; if it gets a non-401
	// it falls through to the well-known path on the same base URL.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		// Return 200 (no WWW-Authenticate) — Discover falls through to the
		// direct well-known fallback.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(buildWellKnownMeta(t,
			"https://provider.example/authorize",
			"https://provider.example/token",
		))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	meta, err := discoverOAuthMetadata(context.Background(), http.DefaultClient, srv.URL)
	require.NoError(t, err)
	require.NotNil(t, meta)
	assert.Equal(t, "https://provider.example/authorize", meta.AuthorizationEndpoint)
	assert.Equal(t, "https://provider.example/token", meta.TokenEndpoint)
}

func TestDiscoverOAuthMetadata_Error(t *testing.T) {
	// Server returns 404 for all paths → Discover returns an error naming
	// the tried URLs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	_, err := discoverOAuthMetadata(context.Background(), http.DefaultClient, srv.URL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/.well-known/oauth-authorization-server")
}

// --- oauthRedirectURI ---

func TestOAuthRedirectURI(t *testing.T) {
	cases := []struct {
		name        string
		externalURL string
		credName    string
		want        string
	}{
		{
			name:        "https no trailing slash",
			externalURL: "https://identityd.example.org",
			credName:    "linear-oauth",
			want:        "https://identityd.example.org/oauth/callback/linear-oauth",
		},
		{
			name:        "https trailing slash trimmed",
			externalURL: "https://identityd.example.org/",
			credName:    "linear-oauth",
			want:        "https://identityd.example.org/oauth/callback/linear-oauth",
		},
		{
			name:        "http localhost",
			externalURL: "http://localhost:8080",
			credName:    "github-oauth",
			want:        "http://localhost:8080/oauth/callback/github-oauth",
		},
		{
			name:        "credname with safe special chars",
			externalURL: "https://e.org",
			credName:    "my-cred-with-dashes",
			want:        "https://e.org/oauth/callback/my-cred-with-dashes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, oauthRedirectURI(tc.externalURL, tc.credName))
		})
	}
}
