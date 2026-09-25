// Package oauth implements OAuth 2.0 + PKCE (RFC 7636) helpers for MCP
// server authentication. It uses only the standard library — no third-party
// OAuth client dependencies.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCE holds the three values produced by NewPKCE.
type PKCE struct {
	// Verifier is the raw code_verifier string (43–128 URL-safe chars).
	Verifier string
	// Challenge is base64url-no-padding(sha256(Verifier)) — sent as code_challenge.
	Challenge string
	// State is a random nonce sent in the authorization request and validated
	// on redirect (16 random bytes → base64url-no-padding).
	State string
}

// NewPKCE generates a fresh PKCE bundle using crypto/rand.
// code_verifier: 32 random bytes → base64url-no-padding (43 chars, within [43,128]).
// code_challenge: base64url-no-padding(sha256(verifier)).
// state: 16 random bytes → base64url-no-padding.
func NewPKCE() (PKCE, error) {
	verifierRaw := make([]byte, 32)
	if _, err := rand.Read(verifierRaw); err != nil {
		return PKCE{}, fmt.Errorf("pkce: generate verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(verifierRaw)

	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	stateRaw := make([]byte, 16)
	if _, err := rand.Read(stateRaw); err != nil {
		return PKCE{}, fmt.Errorf("pkce: generate state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateRaw)

	return PKCE{Verifier: verifier, Challenge: challenge, State: state}, nil
}
