// pkg/platform/identityd/handlers_oauthas.go — identityd acting as an OAuth
// 2.0 AUTHORIZATION SERVER for its own /oauth/authorize + /oauth/token (Tasks
// 7-8), advertised via RFC 8414 metadata and RFC 7591 Dynamic Client
// Registration here.
//
// This is the opposite role from handlers_oauth.go, which makes identityd an
// OAuth CLIENT of upstream MCP servers. The two never share state: a client_id
// minted here is for a tool authenticating TO identityd, not a credential
// identityd holds for some other service.
//
// client_id is STATELESS: it is itself a passthroughlink-signed blob carrying
// the registration ({name, redirect_uris}), so DCR needs no storage and a
// client_id verifies the same on any replica, before or after a restart.
package identityd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/passthroughlink"
)

const (
	// oauthClientIDTTL is how long a DCR'd client_id remains valid. Long
	// enough that a registered local tool keeps working across routine
	// operation; see PurposeOAuthClient's doc for the rotation-overlap
	// rationale.
	oauthClientIDTTL = 2 * 365 * 24 * time.Hour
	// maxClientNameLen caps the sanitized client_name stored in the
	// client_id blob — it rides in a cookie-sized token and is rendered back
	// to the user later, so it is capped rather than left open-ended.
	maxClientNameLen = 64
	// maxRegisterBodyLen caps the POST /oauth/register request body, mirroring
	// handleCLIExchange's 4KB cap on the CLI-exchange JSON endpoint; 8KB gives
	// a few redirect_uris plenty of room without accepting an unbounded body.
	maxRegisterBodyLen = 8 << 10
)

// clientReg is the registration a client_id encodes. Exported-shaped (but
// package-private) so Tasks 7-8's verifyClientID callers can read it.
type clientReg struct {
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// handleOAuthASMetadata handles GET /.well-known/oauth-authorization-server
// (RFC 8414). AuthNone: this document is, by definition, what an unauthenticated
// client fetches first.
func (s *Server) handleOAuthASMetadata(w http.ResponseWriter, _ *http.Request) {
	base := strings.TrimSuffix(s.deps.ExternalBaseURL(), "/")
	writeJSONBody(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
	})
}

// handleOAuthRegister handles POST /oauth/register (RFC 7591 Dynamic Client
// Registration). AuthHandlerManaged: a not-yet-registered client has no
// credential to authenticate with, so this is necessarily an open endpoint;
// what it hands back is a signed, expiring, otherwise-inert blob, not access
// to anything.
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRegisterBodyLen))
	if err := dec.Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "malformed registration body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, raw := range req.RedirectURIs {
		if err := validateRedirectURI(raw); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	name := sanitizeClientName(req.ClientName)
	clientID, err := s.mintClientID(name, req.RedirectURIs)
	if err != nil {
		logger.Info("oauth register: mint client_id failed", "err", err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	writeJSONBody(w, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
	})
}

// validateRedirectURI allows loopback http (127.0.0.1/::1/localhost, any
// port) and https anywhere — OAuth 2.1's public-client rule.
func validateRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("unparseable redirect_uri")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			return nil
		}
		return fmt.Errorf("http redirect_uri must be loopback")
	}
	return fmt.Errorf("redirect_uri scheme must be https or loopback http")
}

// sanitizeClientName strips non-printable runes and '<'/'>' (the client_name
// is later rendered back to a human at the authorize step; this is a
// display-safety net, not a substitute for the renderer's own escaping),
// trims whitespace, defaults an empty result to "unnamed client", and caps
// length at maxClientNameLen.
func sanitizeClientName(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) && r != '<' && r != '>' {
			return r
		}
		return -1
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		cleaned = "unnamed client"
	}
	if len(cleaned) > maxClientNameLen {
		cleaned = cleaned[:maxClientNameLen]
	}
	return cleaned
}

// mintClientID encodes the registration as a signed, expiring blob — the
// client_id IS the registration, so DCR needs no storage and survives
// restarts and replicas.
func (s *Server) mintClientID(name string, redirectURIs []string) (string, error) {
	blob, err := json.Marshal(clientReg{Name: name, RedirectURIs: redirectURIs})
	if err != nil {
		return "", err
	}
	return s.deps.LinkSigner.Mint(passthroughlink.Payload{
		Purpose:   passthroughlink.PurposeOAuthClient,
		BackLink:  string(blob),
		ExpiresAt: time.Now().Add(oauthClientIDTTL).Unix(),
	})
}

// verifyClientID decodes + verifies a client_id minted by mintClientID.
// Tasks 7-8 (/oauth/authorize, /oauth/token) call this to resolve the
// client making the request — a stateless lookup, not a store Get.
func (s *Server) verifyClientID(clientID string) (clientReg, error) {
	var reg clientReg
	p, err := s.deps.LinkSigner.Verify(clientID)
	if err != nil {
		return reg, fmt.Errorf("invalid client_id: %w", err)
	}
	if p.Purpose != passthroughlink.PurposeOAuthClient {
		return reg, fmt.Errorf("client_id has wrong purpose")
	}
	if err := json.Unmarshal([]byte(p.BackLink), &reg); err != nil {
		return reg, fmt.Errorf("client_id payload undecodable: %w", err)
	}
	return reg, nil
}

// oauthError writes an RFC 7591 / RFC 6749 §5.2-shaped error envelope
// ({"error","error_description"}) — distinct from handlers_cli.go's
// jsonError (a bare {"error":"<msg>"} for the CLI-exchange endpoint, which
// predates and isn't an OAuth wire format): pkg/tools/mcp/oauth.Register,
// our own client-side DCR caller, parses exactly this two-field shape out of
// a non-2xx response, so our own server's errors must speak it too.
func oauthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSONBody(w, status, map[string]string{"error": code, "error_description": desc})
}

// writeJSONBody writes body as JSON with the given status. Shared by every
// handler in this file (metadata, registration success, oauthError) since
// none of the package's other JSON helpers (jsonError's single-field shape)
// fit a body with more than one key.
func writeJSONBody(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
