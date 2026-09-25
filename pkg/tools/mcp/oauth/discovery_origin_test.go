package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The authorization server was sourced transitively from a third party's own
// WWW-Authenticate header, with no origin binding and no issuer verification,
// and the resulting authorization_endpoint is then used as a BROWSER REDIRECT
// TARGET from the authenticated portal origin.
//
// Neither RFC check was performed. RFC 9728 §3.3 requires the
// resource_metadata URL to belong to the resource server's own origin, and RFC
// 8414 §3.3 requires the returned issuer to equal the URL the metadata was
// fetched from — which could not be checked even in principle, because
// Metadata had no Issuer field at all.
//
// The attack does not need a hostile MCP server: the header is per-response, so
// a compromised CDN in front of an honest one, or an honest one with a
// response-header-injection bug, is enough. A signed-in user opens
// /link/oauth/<cred>; identityd discovers the attacker's AS, performs Dynamic
// Client Registration at the ATTACKER's registration_endpoint (leaking the
// deployment's external base URL and per-credential callback URI), then 302s
// the user's browser from the AP portal origin to an arbitrary attacker URL
// carrying the AP state token. The user consents on a page that looks like part
// of the flow they started, and an attacker-issued token is filed as their
// credential.
func TestDiscover_RefusesACrossOriginResourceMetadataURL(t *testing.T) {
	var attackerHits int
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attackerHits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resourceServerMeta{
			AuthorizationServers: []string{"https://attacker.invalid/as"},
		})
	}))
	t.Cleanup(attacker.Close)

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			fmt.Sprintf(`Bearer resource_metadata=%q`, attacker.URL+"/rm"))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(mcp.Close)

	_, err := Discover(context.Background(), mcp.Client(), mcp.URL)

	require.Error(t, err, "an off-origin resource_metadata URL must not be followed")
	assert.Zero(t, attackerHits,
		"and it must not be FETCHED either — the request itself is the SSRF and the leak")
}

// RFC 8414 §3.3: the issuer in the returned metadata must equal the
// authorization server the document was fetched from. Without it, an honest
// resource server pointing at a compromised or misconfigured AS URL yields
// endpoints nothing cross-checks.
func TestDiscover_RefusesAuthServerMetadataWhoseIssuerDisagrees(t *testing.T) {
	var as *httptest.Server
	as = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Metadata{
			Issuer:                "https://somewhere.else.invalid",
			AuthorizationEndpoint: as.URL + "/authorize",
			TokenEndpoint:         as.URL + "/token",
		})
	}))
	t.Cleanup(as.Close)

	_, err := fetchAuthServerMetadata(context.Background(), as.Client(),
		as.URL+"/.well-known/oauth-authorization-server", false)

	require.Error(t, err, "metadata whose issuer names another server must be refused")
	assert.ErrorContains(t, err, "issuer")
}

// The honest same-origin path must keep working, or the checks are an outage.
func TestDiscover_SameOriginResourceMetadataAndMatchingIssuerStillWork(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rm":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resourceServerMeta{
				AuthorizationServers: []string{srv.URL},
			})
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Metadata{
				Issuer:                srv.URL,
				AuthorizationEndpoint: srv.URL + "/authorize",
				TokenEndpoint:         srv.URL + "/token",
			})
		default:
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, srv.URL+"/rm"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(srv.Close)

	md, err := Discover(context.Background(), srv.Client(), srv.URL+"/mcp")

	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, srv.URL+"/token", md.TokenEndpoint)
}

// TestDiscover_FallbackFollowsADecoupledIssuer pins the fix for a live
// failure (Profound, ap-desktop 2026-09-17): the MCP server answers the
// unauthenticated probe with a non-401 status (405 here, matching Profound's
// real response), so Discover never takes the WWW-Authenticate/RFC 9728
// branch, and falls back to <mcpURL>/.well-known/oauth-authorization-server
// on the MCP server's OWN origin. That document is real and self-served, but
// names a DIFFERENT issuer — a decoupled auth-server subdomain, the same
// resource-server/auth-server split RFC 9728's authorization_servers[] hop
// already tolerates one path over. The fallback must follow it, once, to the
// issuer's own well-known metadata, and use THAT (self-consistent) document
// rather than refusing outright.
func TestDiscover_FallbackFollowsADecoupledIssuer(t *testing.T) {
	var authSrv *httptest.Server
	authSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Metadata{
			Issuer:                authSrv.URL,
			AuthorizationEndpoint: authSrv.URL + "/authorize",
			TokenEndpoint:         authSrv.URL + "/token",
		})
	}))
	t.Cleanup(authSrv.Close)

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			// Served directly by the MCP server's own origin (mirroring
			// Profound), but its issuer names the separate auth server.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Metadata{
				Issuer:                authSrv.URL,
				AuthorizationEndpoint: authSrv.URL + "/authorize",
				TokenEndpoint:         authSrv.URL + "/token",
			})
		default:
			// Non-401 (405, matching Profound): the WWW-Authenticate/RFC 9728
			// branch is never taken.
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(mcp.Close)

	md, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")

	require.NoError(t, err, "a self-served well-known naming a decoupled issuer must be followed, not refused")
	assert.Equal(t, authSrv.URL+"/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, authSrv.URL+"/token", md.TokenEndpoint)
}

// TestDiscover_FallbackIssuerFollowIsOneHopOnly proves the follow the
// previous test relies on cannot chain: the MCP server's own well-known
// points at a second server, whose OWN well-known metadata itself disagrees
// with ITS origin (names a THIRD server). That third hop must be refused —
// the terminal fetch (followIssuer=false on the recursive call) enforces
// strict self-consistency, exactly as it does for the RFC 9728 branch's
// authorization_servers[0] hop.
func TestDiscover_FallbackIssuerFollowIsOneHopOnly(t *testing.T) {
	var hop2 *httptest.Server
	hop2 = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-authorization-server" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// Disagrees with hop2's own origin too — a second hop, which must not
		// be followed.
		_ = json.NewEncoder(w).Encode(Metadata{
			Issuer:                "https://a-third-server.invalid",
			AuthorizationEndpoint: hop2.URL + "/authorize",
			TokenEndpoint:         hop2.URL + "/token",
		})
	}))
	t.Cleanup(hop2.Close)

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Metadata{
				Issuer:                hop2.URL,
				AuthorizationEndpoint: hop2.URL + "/authorize",
				TokenEndpoint:         hop2.URL + "/token",
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(mcp.Close)

	_, err := Discover(context.Background(), mcp.Client(), mcp.URL+"/mcp")

	require.Error(t, err, "a second issuer hop must be refused, not chased indefinitely")
	assert.ErrorContains(t, err, "issuer")
}

// An AS that omits issuer entirely is a pre-RFC-8414 server, not an attack.
// Refusing it would break real deployments; the origin binding on the
// resource_metadata URL is what carries the security weight, and this only adds
// a check when the server made a claim to check.
func TestDiscover_AnAbsentIssuerIsNotAFailure(t *testing.T) {
	var as *httptest.Server
	as = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"authorization_endpoint": as.URL + "/authorize",
			"token_endpoint":         as.URL + "/token",
		})
	}))
	t.Cleanup(as.Close)

	md, err := fetchAuthServerMetadata(context.Background(), as.Client(),
		as.URL+"/.well-known/oauth-authorization-server", false)

	require.NoError(t, err)
	assert.Equal(t, as.URL+"/authorize", md.AuthorizationEndpoint)
}
