package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Token is the result of a successful token exchange.
type Token struct {
	// AccessToken is the bearer credential presented upstream.
	AccessToken string `json:"access_token"`
	// TokenType is the presentation scheme, normally "Bearer".
	TokenType string `json:"token_type"`
	// ExpiresIn is the token's lifetime in SECONDS from issuance. Zero means
	// the server declared no lifetime, not that the token is already expired.
	ExpiresIn int `json:"expires_in,omitempty"`
	// RefreshToken renews the access token without another user interaction.
	// Empty means this grant cannot be refreshed.
	RefreshToken string `json:"refresh_token,omitempty"`
	// Scope is the space-separated set actually granted, which may be narrower
	// than what was requested.
	Scope string `json:"scope,omitempty"`

	// TokenEndpoint is the token endpoint URL used for this exchange.
	// Populated by Login (not from the JSON body) so callers can persist
	// it for future token refresh (RFC 6749 §6).
	TokenEndpoint string `json:"-"`

	// ClientID is the OAuth client_id used for this exchange.
	// Populated by Login (from LoginOpts.ClientID or DCR result) so
	// callers can persist it alongside the refresh_token.
	ClientID string `json:"-"`

	// ClientSecret is the OAuth client_secret used for this exchange,
	// if any. Populated by Login (from LoginOpts.ClientSecret or the
	// DCR result) so callers can persist it for future refresh —
	// confidential clients (e.g. HubSpot's MCP auth apps) require it
	// on the refresh_token grant. Empty for public clients.
	ClientSecret string `json:"-"`
}

// tokenErrorResponse is the RFC 6749 error shape.
type tokenErrorResponse struct {
	// Error is the machine-readable RFC 6749 error code.
	Error string `json:"error"`
	// ErrorDescription is the server's free-form explanation, if any.
	ErrorDescription string `json:"error_description,omitempty"`
}

// ExchangeCodeParams holds the parameters for a token exchange request.
type ExchangeCodeParams struct {
	Code        string
	RedirectURI string
	ClientID    string
	// ClientSecret is optional; omitted for public clients.
	ClientSecret string
	CodeVerifier string
}

// ExchangeCode exchanges an authorization code for a token (RFC 6749 §4.1.3 +
// RFC 7636 §4.5). Sends Content-Type: application/x-www-form-urlencoded.
func ExchangeCode(ctx context.Context, hc *http.Client, tokenEndpoint string, p ExchangeCodeParams) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", p.Code)
	form.Set("redirect_uri", p.RedirectURI)
	form.Set("client_id", p.ClientID)
	form.Set("code_verifier", p.CodeVerifier)
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("token exchange: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: POST %s: %w", tokenEndpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("token exchange: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e tokenErrorResponse
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("token exchange: server error %q: %s", e.Error, e.ErrorDescription)
		}
		return nil, fmt.Errorf("token exchange: unexpected status %d from %s", resp.StatusCode, tokenEndpoint)
	}

	var tok Token
	if err := json.Unmarshal(raw, &tok); err != nil {
		return nil, fmt.Errorf("token exchange: decode response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("token exchange: response missing access_token")
	}
	return &tok, nil
}
