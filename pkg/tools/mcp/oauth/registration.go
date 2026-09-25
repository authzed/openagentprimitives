package oauth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// registrationRequest is the RFC 7591 Dynamic Client Registration request body.
type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// registrationResponse holds the fields we care about from the RFC 7591 response.
type registrationResponse struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	// Error fields (used when the server returns an error envelope in 200/4xx).
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// Register performs Dynamic Client Registration (RFC 7591) against
// registrationEndpoint. Every URI in redirectURIs is registered; the
// authorization-time URI must byte-match one of them. Pass both
// `http://127.0.0.1:PORT/callback` and `http://localhost:PORT/callback`
// when you don't know which loopback form the server prefers — RFC 8252
// recommends the IP literal but many providers only accept the hostname.
//
// Returns (clientID, clientSecret, err). clientSecret will be empty for
// public clients.
func Register(ctx context.Context, hc *http.Client, registrationEndpoint string, redirectURIs []string) (clientID, clientSecret string, err error) {
	if len(redirectURIs) == 0 {
		return "", "", fmt.Errorf("register: redirectURIs is required and must contain at least one URI")
	}
	reqBody := registrationRequest{
		RedirectURIs:            redirectURIs,
		ClientName:              "agentprimitives oap CLI",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", "", fmt.Errorf("register: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("register: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("register: POST %s: %w", registrationEndpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("register: read response: %w", err)
	}

	var result registrationResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", fmt.Errorf("register: decode response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if result.Error != "" {
			return "", "", fmt.Errorf("register: server error %q: %s", result.Error, result.ErrorDescription)
		}
		return "", "", fmt.Errorf("register: unexpected status %d from %s", resp.StatusCode, registrationEndpoint)
	}

	if result.Error != "" {
		return "", "", fmt.Errorf("register: server error %q: %s", result.Error, result.ErrorDescription)
	}
	if result.ClientID == "" {
		return "", "", fmt.Errorf("register: response missing client_id")
	}

	return result.ClientID, result.ClientSecret, nil
}
