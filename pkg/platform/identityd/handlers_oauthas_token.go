// pkg/platform/identityd/handlers_oauthas_token.go — POST /oauth/token, the
// final leg of identityd's OWN OAuth AUTHORIZATION-SERVER role (see
// handlers_oauthas.go's package doc). Redeems the single-use authorization
// code handlers_oauthas_authorize.go minted at consent for a bearer access
// token, via pkg/web/mcpfront's Minter.
//
// identityd cannot import pkg/web/mcpfront directly: mcpfront is under
// pkg/web, and the import direction in this repo is web -> platform, never
// the reverse (see AGENTS.md). AccessTokenMinter, MintParams and Minted below
// are therefore a MIRROR of mcpfront.Minter.MintAccessToken's signature, not
// an import of it — webd (which can see both packages) adapts its concrete
// *mcpfront.Minter to satisfy AccessTokenMinter structurally, with no shared
// type between the two packages. Deps.Minter is nil until that adapter is
// wired (see webui.go); a nil Minter 503s rather than panicking.
package identityd

import (
	"context"
	"net/http"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

// AccessTokenMinter mints an access token for an approved, validated
// authorization-code exchange. Mirrors
// pkg/web/mcpfront.Minter.MintAccessToken's signature — see the package doc
// above for why this is a mirror rather than an import.
type AccessTokenMinter interface {
	MintAccessToken(ctx context.Context, p MintParams) (Minted, error)
}

// MintParams mirrors pkg/web/mcpfront.MintParams field-for-field.
type MintParams struct {
	Owner        identity.CanonicalUserID
	Role         string
	ScopeClasses []string
	Unfiltered   bool
	ClientName   string
	ClientID     string
}

// Minted mirrors pkg/web/mcpfront.Minted field-for-field.
type Minted struct {
	Value     string
	TokenID   string
	ExpiresAt time.Time
}

// handleOAuthToken handles POST /oauth/token (RFC 6749 §4.1.3): redeems a
// single-use authorization code for a bearer access token. Every validation
// failure returns the SAME 400 invalid_grant (grant_type itself is the one
// exception, at unsupported_grant_type) — there is no oracle telling a caller
// WHICH check failed beyond that first one, matching the no-foothold posture
// the rest of this authorization-server surface already takes.
//
// The code is consumed (single-use, burned either way) BEFORE the
// client_id/redirect_uri/PKCE binding checks run — a mismatched exchange
// still spends the code, per RFC 6749 §4.1.2's "the authorization code MUST
// NOT be used more than once". A replayed or mismatched exchange can never
// succeed on a second attempt with the same code.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	logger := log.FromContext(r.Context())

	if err := r.ParseForm(); err != nil {
		logger.Info("oauth token: ParseForm failed", "err", err.Error())
		oauthError(w, http.StatusBadRequest, "invalid_grant", "could not read the form submission")
		return
	}

	if r.FormValue("grant_type") != "authorization_code" {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code is supported")
		return
	}

	clientID := r.FormValue("client_id")
	reg, err := s.verifyClientID(clientID)
	if err != nil {
		logger.Info("oauth token: invalid client_id", "err", err.Error())
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization grant")
		return
	}

	entry, ok := s.authCodes.Consume(r.FormValue("code"))
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization grant")
		return
	}

	if entry.ClientID != clientID {
		logger.Info("oauth token: client_id mismatch")
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization grant")
		return
	}
	if entry.RedirectURI != r.FormValue("redirect_uri") {
		logger.Info("oauth token: redirect_uri mismatch")
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization grant")
		return
	}
	if !oauth.VerifyS256(r.FormValue("code_verifier"), entry.Challenge) {
		logger.Info("oauth token: PKCE verification failed")
		oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid authorization grant")
		return
	}

	if s.deps.Minter == nil {
		logger.Info("oauth token: no access-token minter wired")
		oauthError(w, http.StatusServiceUnavailable, "server_error", "token minting unavailable")
		return
	}

	minted, err := s.deps.Minter.MintAccessToken(r.Context(), MintParams{
		Owner:        entry.Subject,
		Role:         entry.Role,
		ScopeClasses: entry.ScopeClasses,
		Unfiltered:   entry.Unfiltered,
		ClientName:   reg.Name,
		ClientID:     clientID,
	})
	if err != nil {
		logger.Info("oauth token: mint failed", "err", err.Error())
		oauthError(w, http.StatusInternalServerError, "server_error", "could not mint access token")
		return
	}

	writeJSONBody(w, http.StatusOK, map[string]any{
		"access_token": minted.Value,
		"token_type":   "Bearer",
		"expires_in":   int(time.Until(minted.ExpiresAt).Seconds()),
		"scope":        scopeString(entry),
	})
}

// scopeString renders the RFC 6749 §5.1 `scope` response field for a minted
// token: "role:<role> classes:<comma-joined>", or "role:<role> classes:*"
// when the grant is unfiltered.
func scopeString(e authCodeEntry) string {
	if e.Unfiltered {
		return "role:" + e.Role + " classes:*"
	}
	return "role:" + e.Role + " classes:" + strings.Join(e.ScopeClasses, ",")
}
