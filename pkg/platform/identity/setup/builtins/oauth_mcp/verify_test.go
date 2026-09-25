package oauth_mcp_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/provider"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/setup/builtins/oauth_mcp"
)

// oauthMCPProvider is the catalog entry as shipped: shape oauth, routed to
// this builtin, and declaring no verify: probe.
func oauthMCPProvider(t *testing.T) *provider.Provider {
	t.Helper()
	p, ok := provider.ByID("oauth-mcp")
	require.True(t, ok, "oauth-mcp must be present in the embedded provider catalog")
	return p
}

// TestVerifyNeverClaimsValidWithoutChecking pins the invariant this flow
// violated: it performs NO live check, so it must not report VerifyValid.
//
// Why an honest "unsupported" and not a probe: an OAuth access token minted for
// an MCP server is audience-bound (RFC 8707), and a compliant resource server
// MUST NOT accept a token issued for a different resource — so there is no
// fixed endpoint a catalog-level probe could call. Claiming valid anyway is
// worse than admitting we cannot tell: builtins.VerifyCredential short-circuits
// to this flow, and credupdate.Determine maps VerifyValid to a REFUSAL that
// tells the agent the credential "still authenticates successfully".
func TestVerifyNeverClaimsValidWithoutChecking(t *testing.T) {
	cases := []struct {
		name  string
		value builtins.StoreValue
	}{
		{
			name: "a full, healthy-looking token bundle: unsupported, because nothing was checked",
			value: builtins.StoreValue{OAuth: &builtins.OAuthValue{
				AccessToken:   "at-looks-fine",
				RefreshToken:  "rt-looks-fine",
				ExpiresIn:     3600,
				TokenEndpoint: "https://auth.example.test/token",
				ClientID:      "client-abc",
			}},
		},
		{
			name: "a revoked bundle the provider would reject: unsupported, NOT valid",
			value: builtins.StoreValue{OAuth: &builtins.OAuthValue{
				AccessToken:  "at-revoked",
				RefreshToken: "rt-revoked",
			}},
		},
		{
			name:  "an empty value with no token at all: unsupported, NOT valid",
			value: builtins.StoreValue{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := oauth_mcp.New().Verify(context.Background(), builtins.VerifyRequest{
				Provider: oauthMCPProvider(t),
				Value:    tc.value,
			})
			require.NoError(t, err, "Verify must not error")

			assert.NotEqual(t, builtins.VerifyValid, res.Status,
				"Verify performed no check, so it must never assert the credential is valid")
			assert.Equal(t, builtins.VerifyUnsupported, res.Status,
				"no live verification is possible for an audience-bound MCP token; report unsupported")
			assert.NotEmpty(t, res.Detail,
				"Detail is never empty for a non-valid status (no-silent-errors)")
			assert.Empty(t, res.Subject,
				"nothing was authenticated, so there is no verified subject to report")
		})
	}
}

// TestVerifyMakesNoOutboundCall is the other half of the same invariant: the
// status must be unsupported BECAUSE no probe happened, not in spite of one.
// It also guards the reverse lie — a probe pointed at the wrong endpoint would
// 401 on a perfectly healthy credential and report every one of them dead.
//
// If a genuine per-MCPServer probe is ever added, this test is the deliberate
// trip-wire: it should be rewritten alongside the probe, not deleted quietly.
func TestVerifyMakesNoOutboundCall(t *testing.T) {
	called := false
	oauth_mcp.SetHTTPClient(func() *http.Client {
		called = true
		return http.DefaultClient
	})
	t.Cleanup(func() { oauth_mcp.SetHTTPClient(nil) })

	_, err := oauth_mcp.New().Verify(context.Background(), builtins.VerifyRequest{
		Provider: oauthMCPProvider(t),
		Value:    builtins.StoreValue{OAuth: &builtins.OAuthValue{AccessToken: "at-any"}},
	})
	require.NoError(t, err, "Verify must not error")
	assert.False(t, called,
		"Verify must not reach for an HTTP client: there is no endpoint an audience-bound token can be probed against")
}

// TestVerifyCredentialRoutesOAuthMCPToTheBuiltin proves the fix lands on the
// path production actually takes. VerifyCredential short-circuits to a
// registered builtin (verify.go), so the catalog's absent verify: block is
// never consulted and this flow's return value IS the platform's answer.
func TestVerifyCredentialRoutesOAuthMCPToTheBuiltin(t *testing.T) {
	prov := oauthMCPProvider(t)
	require.Equal(t, "oauth-mcp", prov.Builtin,
		"the catalog entry must route to this builtin, or the flow's verdict is not the one production uses")

	res := builtins.VerifyCredential(context.Background(), prov,
		builtins.StoreValue{OAuth: &builtins.OAuthValue{AccessToken: "at-revoked"}})

	assert.Equal(t, builtins.VerifyUnsupported, res.Status,
		"the choke point every entry point calls must report unsupported for oauth-mcp")
	assert.NotEmpty(t, res.Detail, "Detail is never empty for a non-valid status")
}
