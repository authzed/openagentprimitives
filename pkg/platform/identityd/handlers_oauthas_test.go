package identityd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOAuthASMetadata(t *testing.T) {
	s := newTestServerWithConsent(t) // metadata/register are gated on Consent != nil (Task 7)
	req := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-authorization-server", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var md struct {
		Issuer                   string   `json:"issuer"`
		AuthorizationEndpoint    string   `json:"authorization_endpoint"`
		TokenEndpoint            string   `json:"token_endpoint"`
		RegistrationEndpoint     string   `json:"registration_endpoint"`
		ResponseTypes            []string `json:"response_types_supported"`
		GrantTypes               []string `json:"grant_types_supported"`
		CodeChallengeMethods     []string `json:"code_challenge_methods_supported"`
		TokenEndpointAuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &md))
	base := strings.TrimSuffix(md.Issuer, "/")
	assert.Equal(t, base+"/oauth/authorize", md.AuthorizationEndpoint)
	assert.Equal(t, base+"/oauth/token", md.TokenEndpoint)
	assert.Equal(t, base+"/oauth/register", md.RegistrationEndpoint)
	assert.Equal(t, []string{"code"}, md.ResponseTypes)
	assert.Equal(t, []string{"authorization_code"}, md.GrantTypes)
	assert.Equal(t, []string{"S256"}, md.CodeChallengeMethods)
	assert.Equal(t, []string{"none"}, md.TokenEndpointAuthMethods)
}

func TestDynamicClientRegistration(t *testing.T) {
	s := newTestServerWithConsent(t)
	body := `{"client_name":"demo local tool","redirect_uris":["http://127.0.0.1:7777/callback"],"token_endpoint_auth_method":"none"}`
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	var resp struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.ClientID)

	reg, err := s.verifyClientID(resp.ClientID)
	require.NoError(t, err)
	assert.Equal(t, "demo local tool", reg.Name)
	assert.Equal(t, []string{"http://127.0.0.1:7777/callback"}, reg.RedirectURIs)
}

func TestDynamicClientRegistrationRejections(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{name: "no redirect_uris: 400", body: `{"client_name":"x"}`},
		{name: "non-loopback http redirect: 400", body: `{"client_name":"x","redirect_uris":["http://evil.test/cb"]}`},
		{name: "malformed JSON: 400", body: `{"client_name":`},
	}
	s := newTestServerWithConsent(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}
