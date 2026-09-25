// Package fakeoauth provides an in-process OAuth/OIDC provider for
// tests + the Slice-3 OAuth E2E. It implements just enough of the
// OAuth dance to satisfy pkg/tools/mcp/oauth.Discover, the authorize+token
// endpoints, and dynamic client registration. No consent UI: /authorize
// auto-grants and redirects.
package fakeoauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Server is the fake provider. Lifecycle: NewServer → use s.URL() as the
// MCPServer's Spec.Server.URL → s.Close() on cleanup.
type Server struct {
	ts *httptest.Server
	mu sync.Mutex

	// codes maps issued auth-code → codeRecord.
	codes map[string]codeRecord

	// clients maps registered client_id → clientRecord.
	clients map[string]clientRecord

	// scripted token response. Set via SetTokenResponse.
	tokenResponse *TokenResponse

	// recorded requests for test assertions.
	recorded []RecordedRequest

	// counter for unique IDs, used by uniqueID.
	counter atomic.Uint64
}

type codeRecord struct {
	RedirectURI   string
	CodeChallenge string
	ClientID      string
}

type clientRecord struct {
	RedirectURIs []string
}

// RecordedRequest captures path + method + merged query/form values for one
// request the fake handled.
type RecordedRequest struct {
	Path   string
	Method string
	// Values contains merged URL query params and POST form fields.
	Values url.Values
	At     time.Time
}

// TokenResponse is the canned response /token returns. If nil, /token
// returns a default response with a random access_token and refresh_token.
type TokenResponse struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	ExpiresIn    int // seconds
	Scope        string
}

// NewServer starts a fake provider. Stop with srv.Close().
func NewServer() *Server {
	s := &Server{
		codes:   map[string]codeRecord{},
		clients: map[string]clientRecord{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", s.handleMetadata)
	mux.HandleFunc("/authorize", s.handleAuthorize)
	mux.HandleFunc("/token", s.handleToken)
	mux.HandleFunc("/register", s.handleRegister)
	// Catch-all root. Two roles:
	//   - GET → 200, so OAuth Discover's initial probe doesn't get a
	//     connection error and falls through to the /.well-known path.
	//   - POST (JSON-RPC) → minimal MCP `initialize` + `tools/list`
	//     responses. The Slice-3 OAuth E2E scenario sets MCPServer.Spec.
	//     Server.URL to fakeoauth's URL (so OAuth discovery lands here),
	//     and the in-process runner factory's MCP probe hits the same
	//     URL before spawning. Without an MCP-shaped response the probe
	//     fails and the AgentSession reconcile returns mid-flow,
	//     dropping the in-memory CredentialsReady=True the un-park step
	//     just set. Returning an empty tools list keeps the probe
	//     succeeding without exercising real tool dispatch (the
	//     scenario asserts only on the OAuth dance).
	mux.HandleFunc("/", s.handleRoot)
	s.ts = httptest.NewServer(mux)
	return s
}

// handleRoot serves the catch-all root: GET → 200; POST with a
// JSON-RPC body → a minimal MCP response sufficient for mcpprobe to
// finish its initialize/tools/list handshake.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusOK)
		return
	}
	var req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Method  string `json:"method"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Non-JSON POST — just return 200 with no body. mcpprobe's
		// initialize is a JSON-RPC request, so a non-RPC POST means
		// some other actor; nothing useful we can do here.
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch req.Method {
	case "initialize":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result": map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fakeoauth-mcp-stub", "version": "0"},
			},
		})
	case "tools/list":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{"tools": []map[string]any{}},
		})
	default:
		// Unknown method — return an empty result. mcpprobe doesn't
		// drive arbitrary methods, so this is a defensive fallback.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{},
		})
	}
}

// URL returns the base URL the test should pass to MCPServer.Spec.Server.URL.
func (s *Server) URL() string { return s.ts.URL }

// Client returns the underlying httptest.Server's *http.Client. Useful
// for E2E scenarios that need to install identityd's OAuth HTTP-client
// seam (newOAuthHTTPClient) without going through safehttp.Client —
// safehttp refuses loopback destinations, which httptest always binds.
func (s *Server) Client() *http.Client { return s.ts.Client() }

// Close shuts down the fake.
func (s *Server) Close() { s.ts.Close() }

// SetTokenResponse scripts what /token returns on the next exchange.
// Pass nil to revert to the default random-token behaviour.
func (s *Server) SetTokenResponse(t *TokenResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenResponse = t
}

// RecordedRequests returns a snapshot of every request the fake handled.
func (s *Server) RecordedRequests() []RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RecordedRequest, len(s.recorded))
	copy(out, s.recorded)
	return out
}

// Reset clears all in-memory state (codes, clients, recorded requests).
// Scripted token response is preserved intentionally so tests can call
// SetTokenResponse before Reset and still have it apply.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes = map[string]codeRecord{}
	s.clients = map[string]clientRecord{}
	s.recorded = nil
}

// handleMetadata serves /.well-known/oauth-authorization-server (RFC 8414).
// The schema matches pkg/tools/mcp/oauth.Metadata exactly:
//
//	authorization_endpoint, token_endpoint, registration_endpoint, scopes_supported.
func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	meta := map[string]any{
		"authorization_endpoint":           s.ts.URL + "/authorize",
		"token_endpoint":                   s.ts.URL + "/token",
		"registration_endpoint":            s.ts.URL + "/register",
		"scopes_supported":                 []string{"read", "write"},
		"code_challenge_methods_supported": []string{"S256"},
		"response_types_supported":         []string{"code"},
		"grant_types_supported":            []string{"authorization_code", "refresh_token"},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(meta); err != nil {
		// Encoding to a ResponseWriter only fails after headers are sent; no
		// useful HTTP status can be set at this point.
		return
	}
}

// handleAuthorize serves GET /authorize. Validates required PKCE params,
// auto-grants without a consent UI, and immediately redirects to redirect_uri
// with a fresh code and the echoed state.
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	clientID := r.Form.Get("client_id")
	redirectURI := r.Form.Get("redirect_uri")
	state := r.Form.Get("state")
	codeChallenge := r.Form.Get("code_challenge")
	codeChallengeMethod := r.Form.Get("code_challenge_method")

	if clientID == "" || redirectURI == "" || codeChallenge == "" {
		http.Error(w, "invalid_request: client_id, redirect_uri, and code_challenge are required", http.StatusBadRequest)
		return
	}
	if codeChallengeMethod != "S256" {
		http.Error(w, "invalid_request: only S256 code_challenge_method is supported", http.StatusBadRequest)
		return
	}

	code := s.uniqueID("code")
	s.mu.Lock()
	s.codes[code] = codeRecord{
		RedirectURI:   redirectURI,
		CodeChallenge: codeChallenge,
		ClientID:      clientID,
	}
	s.mu.Unlock()

	u, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "invalid_request: bad redirect_uri: "+err.Error(), http.StatusBadRequest)
		return
	}
	q := u.Query()
	q.Set("code", code)
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// handleToken serves POST /token (authorization_code grant with PKCE).
// Validates the code was issued by the fake, verifies the S256 PKCE
// code_verifier, and returns the scripted or default token response.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if r.Method != http.MethodPost {
		http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form: "+err.Error(), http.StatusBadRequest)
		return
	}
	grantType := r.Form.Get("grant_type")
	if grantType != "authorization_code" {
		writeJSONError(w, "unsupported_grant_type", "only authorization_code is supported", http.StatusBadRequest)
		return
	}
	code := r.Form.Get("code")
	verifier := r.Form.Get("code_verifier")
	redirectURI := r.Form.Get("redirect_uri")
	if code == "" || verifier == "" || redirectURI == "" {
		writeJSONError(w, "invalid_request", "code, code_verifier, and redirect_uri are required", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	rec, ok := s.codes[code]
	if ok {
		delete(s.codes, code) // single-use
	}
	tokResp := s.tokenResponse
	s.mu.Unlock()

	if !ok {
		writeJSONError(w, "invalid_grant", "unknown or expired authorization code", http.StatusBadRequest)
		return
	}
	if rec.RedirectURI != redirectURI {
		writeJSONError(w, "invalid_grant", "redirect_uri mismatch", http.StatusBadRequest)
		return
	}

	// Verify PKCE: base64url-no-padding(sha256(verifier)) must equal the
	// recorded challenge. This is the same computation as pkg/tools/mcp/oauth/pkce.go.
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	if computed != rec.CodeChallenge {
		writeJSONError(w, "invalid_grant", "PKCE code_verifier does not match code_challenge", http.StatusBadRequest)
		return
	}

	if tokResp == nil {
		tokResp = &TokenResponse{
			AccessToken:  "fake-access-" + s.uniqueID("at"),
			RefreshToken: "fake-refresh-" + s.uniqueID("rt"),
			TokenType:    "Bearer",
			ExpiresIn:    3600,
		}
	}

	body := map[string]any{
		"access_token": tokResp.AccessToken,
		"token_type":   tokResp.TokenType,
	}
	if tokResp.RefreshToken != "" {
		body["refresh_token"] = tokResp.RefreshToken
	}
	if tokResp.ExpiresIn != 0 {
		body["expires_in"] = tokResp.ExpiresIn
	}
	if tokResp.Scope != "" {
		body["scope"] = tokResp.Scope
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		return
	}
}

// handleRegister serves POST /register (RFC 7591 Dynamic Client Registration).
// Validates the request shape and returns a generated client_id. No
// client_secret is returned — this is a public client (PKCE-only).
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	s.record(r)
	if r.Method != http.MethodPost {
		http.Error(w, "method_not_allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, "invalid_client_metadata", "invalid request payload: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeJSONError(w, "invalid_client_metadata", "redirect_uris is required", http.StatusBadRequest)
		return
	}

	clientID := "fake-client-" + s.uniqueID("reg")
	s.mu.Lock()
	s.clients[clientID] = clientRecord{RedirectURIs: req.RedirectURIs}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"client_id":     clientID,
		"redirect_uris": req.RedirectURIs,
	}); err != nil {
		return
	}
}

// record appends a RecordedRequest for the given HTTP request. Merges URL
// query params and POST form fields into a single Values map so callers
// can assert on either without knowing which encoding was used.
func (s *Server) record(r *http.Request) {
	// Merge query params first, then POST form fields.
	values := url.Values{}
	for k, v := range r.URL.Query() {
		values[k] = v
	}
	// ParseForm is idempotent; the handler may have already called it.
	if err := r.ParseForm(); err == nil {
		for k, v := range r.PostForm {
			values[k] = v
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recorded = append(s.recorded, RecordedRequest{
		Path:   r.URL.Path,
		Method: r.Method,
		Values: values,
		At:     time.Now(),
	})
}

// uniqueID returns a unique string of the form "<prefix>-<hex>" suitable for
// use as a code, client_id, or token. Uses crypto/rand for the random bytes.
func (s *Server) uniqueID(prefix string) string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// Fallback to counter-based ID if rand fails (shouldn't happen in tests).
		n := s.counter.Add(1)
		return prefix + "-" + base64.RawURLEncoding.EncodeToString([]byte{byte(n)})
	}
	return prefix + "-" + base64.RawURLEncoding.EncodeToString(raw)
}

// writeJSONError writes an RFC 6749-style JSON error response.
func writeJSONError(w http.ResponseWriter, code, desc string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	_ = json.NewEncoder(w).Encode(body)
}
