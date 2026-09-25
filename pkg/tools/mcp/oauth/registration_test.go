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

// TestRegister covers the four shapes of registration response Register
// must handle: a confidential client (id+secret), a public client (id only),
// an RFC 7591 error envelope, and a malformed response missing client_id.
// Each row stands up an httptest server with its own handler, calls
// Register, and runs row-specific assertions.
func TestRegister(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		check   func(t *testing.T, clientID, clientSecret string, err error)
	}{
		{
			name: "confidential client: returns client_id + client_secret",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					http.Error(w, "method", http.StatusMethodNotAllowed)
					return
				}
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					http.Error(w, "content-type", http.StatusBadRequest)
					return
				}
				var req registrationRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, "decode", http.StatusBadRequest)
					return
				}
				if len(req.RedirectURIs) == 0 {
					http.Error(w, "no redirect_uris", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(registrationResponse{
					ClientID:     "client-abc",
					ClientSecret: "secret-xyz",
				})
			},
			check: func(t *testing.T, id, secret string, err error) {
				require.NoError(t, err, "Register")
				assert.Equal(t, "client-abc", id)
				assert.Equal(t, "secret-xyz", secret)
			},
		},
		{
			name: "public client: returns client_id only, secret stays empty",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(registrationResponse{
					ClientID: "pub-client",
				})
			},
			check: func(t *testing.T, id, secret string, err error) {
				require.NoError(t, err, "Register")
				assert.Equal(t, "pub-client", id)
				assert.Empty(t, secret, "public client must not have a secret")
			},
		},
		{
			name: "RFC 7591 error envelope: surfaces error code in returned error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(registrationResponse{
					Error:            "invalid_redirect_uri",
					ErrorDescription: "redirect URI not allowed",
				})
			},
			check: func(t *testing.T, _, _ string, err error) {
				require.Error(t, err, "expected error envelope")
				assert.Contains(t, err.Error(), "invalid_redirect_uri")
			},
		},
		{
			name: "200 but missing client_id: error mentions client_id",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{})
			},
			check: func(t *testing.T, _, _ string, err error) {
				require.Error(t, err, "missing client_id should error")
				assert.Contains(t, err.Error(), "client_id")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)
			id, secret, err := Register(context.Background(), http.DefaultClient, srv.URL,
				[]string{"http://127.0.0.1:9999/callback"})
			tc.check(t, id, secret, err)
		})
	}
}
