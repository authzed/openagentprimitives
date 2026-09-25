package idjag_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation/idjag"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// twoLegServer stands up an IdP token endpoint (leg 1) and a resource
// server whose RFC 8414 metadata points its AS token endpoint back at
// itself (leg 2). Returns the resource server base URL, the IdP token
// endpoint URL, and pointers to the recorded form maps.
func twoLegServer(t *testing.T, idjagTok, upstreamTok string) (resourceURL, idpTokenEndpoint string, leg1Form, leg2Form *map[string]string) {
	t.Helper()
	return twoLegServerLeg2Body(t, idjagTok, map[string]any{
		"access_token": upstreamTok, "expires_in": 3600, "token_type": "Bearer",
	})
}

// twoLegServerLeg2Body is twoLegServer with full control over the leg-2 token
// response body, so tests can model an authorization server that omits the
// RFC 6749 §5.1 OPTIONAL expires_in field.
func twoLegServerLeg2Body(t *testing.T, idjagTok string, leg2Body map[string]any) (resourceURL, idpTokenEndpoint string, leg1Form, leg2Form *map[string]string) {
	t.Helper()
	var leg1, leg2 map[string]string

	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		leg1 = formMap(r)
		writeJSON(t, w, map[string]any{"access_token": idjagTok, "issued_token_type": "urn:ietf:params:oauth:token-type:id-jag", "token_type": "N_A"})
	}))
	t.Cleanup(idp.Close)

	var res *httptest.Server
	res = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			writeJSON(t, w, map[string]any{"token_endpoint": res.URL + "/token", "authorization_endpoint": res.URL + "/authorize"})
		case "/token":
			require.NoError(t, r.ParseForm())
			leg2 = formMap(r)
			writeJSON(t, w, leg2Body)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(res.Close)

	return res.URL, idp.URL + "/token", &leg1, &leg2
}

func TestMint_TwoLeggedExchange_ReturnsUpstreamToken(t *testing.T) {
	resourceURL, idpTokenEndpoint, leg1, leg2 := twoLegServer(t, "the-id-jag", "upstream-access-token")

	m := idjag.New(http.DefaultClient) // DefaultClient: httptest uses loopback
	got, err := m.Mint(t.Context(), federation.MintRequest{
		Subject: federation.SubjectMaterial{
			Token:            sensitive.NewSensitiveValue([]byte("user-id-token")),
			IdPTokenEndpoint: idpTokenEndpoint,
			ClientID:         "ap-client",
			ClientSecret:     "ap-secret",
		},
		Resource:          "linear-resource-id",
		ResourceServerURL: resourceURL,
	})
	require.NoError(t, err)
	assert.Equal(t, "upstream-access-token", string(got.AccessToken.UnderlyingValue()))
	assert.False(t, got.ExpiresAt.IsZero(), "expiry must be stamped from expires_in")

	// Leg 1 was a token-exchange carrying the subject token + resource.
	assert.Equal(t, "urn:ietf:params:oauth:grant-type:token-exchange", (*leg1)["grant_type"])
	assert.Equal(t, "user-id-token", (*leg1)["subject_token"])
	assert.Equal(t, "linear-resource-id", (*leg1)["resource"])
	assert.Equal(t, "urn:ietf:params:oauth:token-type:id-jag", (*leg1)["requested_token_type"])
	// Leg 2 presented the ID-JAG as a jwt-bearer assertion.
	assert.Equal(t, "urn:ietf:params:oauth:grant-type:jwt-bearer", (*leg2)["grant_type"])
	assert.Equal(t, "the-id-jag", (*leg2)["assertion"])
}

// A leg-2 authorization server may legally omit expires_in (RFC 6749 §5.1
// makes it OPTIONAL). The minted token must still carry a bounded expiry: the
// broker's cache treats a zero ExpiresAt as "never expires"
// (pkg/platform/identity/broker/inproc/broker.go), so a zero here would pin a
// short-lived federated token in cache for the life of the process — the exact
// staleness that makes mid-session revocation unenforceable.
func TestMint_Leg2WithoutUsableExpiresIn_StampsBoundedDefaultTTL(t *testing.T) {
	cases := []struct {
		name     string
		leg2Body map[string]any
	}{
		{
			name:     "expires_in omitted: bounded default TTL, never zero",
			leg2Body: map[string]any{"access_token": "upstream", "token_type": "Bearer"},
		},
		{
			name:     "expires_in zero: bounded default TTL, never zero",
			leg2Body: map[string]any{"access_token": "upstream", "expires_in": 0, "token_type": "Bearer"},
		},
		{
			name:     "expires_in negative: bounded default TTL, never zero",
			leg2Body: map[string]any{"access_token": "upstream", "expires_in": -1, "token_type": "Bearer"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resourceURL, idpTokenEndpoint, _, _ := twoLegServerLeg2Body(t, "the-id-jag", tc.leg2Body)

			before := time.Now()
			got, err := idjag.New(http.DefaultClient).Mint(t.Context(), federation.MintRequest{
				Subject: federation.SubjectMaterial{
					Token:            sensitive.NewSensitiveValue([]byte("user-id-token")),
					IdPTokenEndpoint: idpTokenEndpoint,
					ClientID:         "ap-client",
				},
				Resource:          "linear-resource-id",
				ResourceServerURL: resourceURL,
			})
			require.NoError(t, err)
			after := time.Now()

			assert.False(t, got.ExpiresAt.IsZero(),
				"zero ExpiresAt means never-expires to the broker cache; a federated token must never be cached forever")
			// Mint stamped now+DefaultTokenTTL at some instant in [before, after],
			// so the expiry must land in [before+TTL, after+TTL]. Bracketing both
			// sides pins the TTL exactly rather than merely asserting "non-zero".
			assert.False(t, got.ExpiresAt.Before(before.Add(idjag.DefaultTokenTTL)),
				"expiry must be at least DefaultTokenTTL (%s) out", idjag.DefaultTokenTTL)
			assert.False(t, got.ExpiresAt.After(after.Add(idjag.DefaultTokenTTL)),
				"expiry must be bounded by DefaultTokenTTL (%s)", idjag.DefaultTokenTTL)
		})
	}
}

func TestMint_Leg1Failure_IsSurfacedAsTokenExchangeError(t *testing.T) {
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"access_denied"}`, http.StatusForbidden)
	}))
	t.Cleanup(idp.Close)

	m := idjag.New(http.DefaultClient)
	_, err := m.Mint(t.Context(), federation.MintRequest{
		Subject:           federation.SubjectMaterial{Token: sensitive.NewSensitiveValue([]byte("t")), IdPTokenEndpoint: idp.URL},
		Resource:          "r",
		ResourceServerURL: "http://127.0.0.1:1", // never reached
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token-exchange", "error must name the failing leg")
}

// --- helpers ---
func formMap(r *http.Request) map[string]string {
	m := map[string]string{}
	for k := range r.Form {
		m[k] = r.Form.Get(k)
	}
	return m
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(v))
}
