package oauth_mcp_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeNoDCRServer is an httptest server whose authorization-server metadata
// OMITS registration_endpoint — the trigger oauth.Login uses to return
// ErrNoDynamicRegistration. /authorize + /token still behave like a normal
// OAuth provider so the second login attempt (with the prompted-for
// client_id / client_secret) can complete.
//
// /token echoes back the client credentials it was called with, so a test can
// prove the second exchange authenticated as the client the USER pasted rather
// than as anything the flow invented.
func fakeNoDCRServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewUnstartedServer(mux)

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate",
			`Bearer realm="mcp", resource_metadata="`+srv.URL+`/resource-meta"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/resource-meta", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_servers": []string{srv.URL},
		})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
			// NO registration_endpoint — DCR unavailable.
		})
	})
	mux.HandleFunc("/authorize", redirectWithCode)
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": noDCRAccessToken,
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	})
	srv.Start()
	return srv
}

// driveBrowser GETs the authorization URL so the localhost callback fires
// without a real browser, mirroring the helper in mcp_oauth_test.go.
func driveBrowser(authURL string) error {
	resp, err := http.Get(authURL) //nolint:noctx // test only
	if err != nil {
		return fmt.Errorf("drive browser: %w", err)
	}
	_ = resp.Body.Close()
	return nil
}
