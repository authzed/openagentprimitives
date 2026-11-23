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
		// wantErr, when set, is the OAuth error code the body must carry.
		wantErr string
	}{
		{name: "no redirect_uris: 400", body: `{"client_name":"x"}`},
		{name: "non-loopback http redirect: 400", body: `{"client_name":"x","redirect_uris":["http://evil.test/cb"]}`, wantErr: "invalid_redirect_uri"},
		{name: "malformed JSON: 400", body: `{"client_name":`},
		{name: "redirect with a fragment: 400", body: `{"client_name":"x","redirect_uris":["https://client.example/cb#frag"]}`, wantErr: "invalid_redirect_uri"},
		{name: "redirect with an empty fragment: 400", body: `{"client_name":"x","redirect_uris":["https://client.example/cb#"]}`, wantErr: "invalid_redirect_uri"},
		{name: "redirect with userinfo: 400", body: `{"client_name":"x","redirect_uris":["https://trusted.example@client.example/cb"]}`, wantErr: "invalid_redirect_uri"},
		{name: "redirect with no host: 400", body: `{"client_name":"x","redirect_uris":["https:///cb"]}`, wantErr: "invalid_redirect_uri"},
		{name: "opaque redirect: 400", body: `{"client_name":"x","redirect_uris":["https:client.example/cb"]}`, wantErr: "invalid_redirect_uri"},
		{name: "one bad redirect among good ones: 400", body: `{"client_name":"x","redirect_uris":["http://127.0.0.1:7777/cb","https://client.example/cb#frag"]}`, wantErr: "invalid_redirect_uri"},
	}
	s := newTestServerWithConsent(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			if tc.wantErr != "" {
				var body map[string]string
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, tc.wantErr, body["error"])
			}
		})
	}
}

func TestValidateRedirectURI(t *testing.T) {
	cases := []struct {
		name, uri string
		wantErr   string // substring; empty means accepted
	}{
		{name: "https anywhere: accepted", uri: "https://client.example/cb"},
		{name: "https with port, path and query: accepted", uri: "https://client.example:8443/oauth/cb?source=cli"},
		{name: "loopback http IPv4: accepted", uri: "http://127.0.0.1:7777/callback"},
		{name: "loopback http localhost: accepted", uri: "http://localhost:7777/callback"},
		{name: "loopback http IPv6: accepted", uri: "http://[::1]:7777/callback"},
		{name: "fragment: rejected", uri: "https://client.example/cb#x", wantErr: "fragment"},
		{name: "bare trailing #: rejected", uri: "https://client.example/cb#", wantErr: "fragment"},
		{name: "fragment on loopback: rejected", uri: "http://127.0.0.1:7777/cb#x", wantErr: "fragment"},
		{name: "userinfo: rejected", uri: "https://user@client.example/cb", wantErr: "userinfo"},
		{name: "userinfo with password: rejected", uri: "https://user:pw@client.example/cb", wantErr: "userinfo"},
		{name: "empty userinfo: rejected", uri: "https://@client.example/cb", wantErr: "userinfo"},
		{name: "empty host: rejected", uri: "https:///cb", wantErr: "host"},
		{name: "port but no host: rejected", uri: "https://:8443/cb", wantErr: "host"},
		{name: "opaque: rejected", uri: "https:client.example/cb", wantErr: "host"},
		{name: "non-loopback http: rejected", uri: "http://client.example/cb", wantErr: "loopback"},
		{name: "custom scheme: rejected", uri: "demoapp://callback", wantErr: "scheme"},
		{name: "relative: rejected", uri: "/cb", wantErr: "host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRedirectURI(tc.uri)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
