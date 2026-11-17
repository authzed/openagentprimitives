package chatgpt

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginValidatesIdentityAndPersistsPrivateRegistration(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, nonce, scope string
		wantError          bool
	}{
		{name: "valid identity and plan permission", scope: directScope},
		{name: "wrong nonce is rejected", nonce: "wrong", scope: directScope, wantError: true},
		{name: "identity only is rejected", scope: "openid email", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var authParams url.Values
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/jwks" {
					require.NoError(t, json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}}))
					return
				}
				require.NoError(t, r.ParseForm())
				assert.Equal(t, "oaiapp_test", r.Form.Get("client_id"))
				assert.Equal(t, resource, r.Form.Get("resource"))
				assert.NotEmpty(t, r.Form.Get("code_verifier"))
				assert.Equal(t, authParams.Get("redirect_uri"), r.Form.Get("redirect_uri"))
				nonce := authParams.Get("nonce")
				if tc.nonce != "" {
					nonce = tc.nonce
				}
				id, e := jwt.Signed(signer).Claims(jwt.Claims{Issuer: issuer, Subject: "subject-one", Audience: jwt.Audience{"oaiapp_test"}, Expiry: jwt.NewNumericDate(time.Now().Add(time.Hour))}).Claims(map[string]any{"nonce": nonce, "email": "person@example.test"}).Serialize()
				require.NoError(t, e)
				require.NoError(t, json.NewEncoder(w).Encode(tokenResponse{AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: id, ExpiresIn: 3600, TokenType: "Bearer", Scope: tc.scope}))
			}))
			defer server.Close()
			c := New(t.TempDir())
			c.tokenURL = server.URL + "/token"
			c.jwksURL = server.URL + "/jwks"
			got, err := c.Login(context.Background(), "", func(raw string) error {
				u, e := url.Parse(raw)
				if e != nil {
					return e
				}
				authParams = u.Query()
				assert.Equal(t, "dynamic_agent_client", authParams.Get("client_id"))
				assert.Equal(t, "S256", authParams.Get("code_challenge_method"))
				assert.NotEmpty(t, authParams.Get("ext_agent_host_id"))
				bad, e := http.Get(authParams.Get("redirect_uri") + "?state=wrong&code=ignored")
				if e != nil {
					return e
				}
				assert.Equal(t, 400, bad.StatusCode)
				if e = bad.Body.Close(); e != nil {
					return e
				}
				callback := authParams.Get("redirect_uri") + "?" + url.Values{"state": {authParams.Get("state")}, "code": {"code"}, "client_id": {"oaiapp_test"}}.Encode()
				response, e := http.Get(callback)
				if e != nil {
					return e
				}
				return response.Body.Close()
			})
			if tc.wantError {
				require.Error(t, err)
				accounts, e := c.Store.Accounts(context.Background())
				require.NoError(t, e)
				assert.Empty(t, accounts)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "subject-one", got.Subject)
			saved, e := c.Store.Accounts(context.Background())
			require.NoError(t, e)
			require.Len(t, saved, 1)
			assert.Equal(t, got.ClientID, saved[0].ClientID)
			assert.Equal(t, got.RefreshToken, saved[0].RefreshToken)
			assert.WithinDuration(t, got.ExpiresAt, saved[0].ExpiresAt, time.Millisecond)
			info, e := os.Stat(filepath.Join(c.Store.Dir, "accounts.json"))
			require.NoError(t, e)
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
		})
	}
}

func TestConcurrentRefreshUsesLatestReplacementAcrossClients(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.NoError(t, r.ParseForm())
		assert.Equal(t, "refresh-old", r.Form.Get("refresh_token"))
		assert.Equal(t, "oaiapp_test", r.Form.Get("client_id"))
		assert.Equal(t, resource, r.Form.Get("resource"))
		assert.Empty(t, r.Form.Get("scope"))
		require.NoError(t, json.NewEncoder(w).Encode(tokenResponse{AccessToken: "access-new", RefreshToken: "refresh-new", TokenType: "Bearer", ExpiresIn: 3600, Scope: directScope}))
	}))
	defer server.Close()
	dir := t.TempDir()
	c := New(dir)
	require.NoError(t, c.Store.transaction(context.Background(), func(st *state) error {
		st.Accounts["oaiapp_test"] = Account{ClientID: "oaiapp_test", Subject: "one", RefreshToken: "refresh-old", ExpiresAt: time.Now().Add(-time.Minute), Scope: directScope}
		return nil
	}))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := New(dir)
			client.tokenURL = server.URL
			a, e := client.Access(context.Background(), "oaiapp_test")
			if e == nil && a.RefreshToken != "refresh-new" {
				e = fmt.Errorf("did not read replacement refresh token")
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		require.NoError(t, e)
	}
	assert.Equal(t, int32(1), calls.Load())
}

func TestTokenErrorsDoNotExposeResponseBodyOrCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "secret-refresh-token", 400) }))
	defer server.Close()
	c := New(t.TempDir())
	c.tokenURL = server.URL
	_, err := c.exchange(context.Background(), url.Values{"refresh_token": {"secret-refresh-token"}})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "secret-refresh-token")
}
