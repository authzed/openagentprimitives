// Package oidctest provides a fake OIDC issuer (httptest.Server) for
// testing the oidc and google idp kinds. The server handles discovery,
// JWKS, and token exchange using a test RSA key.
//
// This package is NOT safe for production use. Methods panic rather than
// returning errors to keep test setup concise.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const keyID = "test-key-1"

// FakeIssuer is an httptest.Server that speaks the OIDC discovery +
// JWKS + token-exchange protocols, backed by a test RSA key pair.
//
// ClaimsFunc is called on every POST /token request; it returns the
// custom claims map merged into the standard set (iss, aud, iat, exp,
// nonce). If ClaimsFunc is nil, DefaultClaims is used instead.
type FakeIssuer struct {
	// Server is the underlying httptest.Server. Close it with
	// t.Cleanup(fi.Server.Close).
	Server *httptest.Server

	// DefaultClaims is merged with the standard claims (iss, aud, iat,
	// exp, nonce) on each token exchange when ClaimsFunc is nil. Callers
	// may replace it or use ClaimsFunc for per-request variation.
	DefaultClaims map[string]any

	// ClaimsFunc, when non-nil, overrides DefaultClaims — called per token
	// request. Receives the clientID and nonce so callers can set aud/nonce.
	ClaimsFunc func(clientID, nonce string) map[string]any

	// TokenOverride, when non-nil, is called with the clientID and nonce
	// extracted from the /token request. Its return value is used as the
	// id_token in the response verbatim, bypassing SignToken entirely.
	// Intended for tests that need to inject a token signed by a key NOT
	// in the issuer's JWKS (e.g. signature-verification failure tests).
	TokenOverride func(clientID, nonce string) string

	priv            *rsa.PrivateKey
	mu              sync.RWMutex
	keyID           string
	suppressIDToken bool
	refreshToken    string
}

// New creates a FakeIssuer with a fresh RSA-2048 key pair and starts it.
// The caller is responsible for calling srv.Server.Close() — typically
// via t.Cleanup(fi.Server.Close).
func New() *FakeIssuer {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic("oidctest: rsa.GenerateKey: " + err.Error())
	}
	fi := &FakeIssuer{
		priv:  priv,
		keyID: keyID,
		DefaultClaims: map[string]any{
			"email":          "alice@example.com",
			"email_verified": true,
			"name":           "Alice",
		},
	}
	fi.Server = httptest.NewServer(fi)
	return fi
}

// URL returns the issuer base URL (same as Server.URL).
func (fi *FakeIssuer) URL() string { return fi.Server.URL }

// Client returns the HTTP client for the underlying httptest.Server.
// Callers should pass oidc.ClientContext(ctx, fi.Client()) so that
// go-oidc routes discovery + JWKS fetches through the test server.
func (fi *FakeIssuer) Client() *http.Client { return fi.Server.Client() }

// SignTokenWithKey signs a JSON payload as a compact RS256 JWT using an
// arbitrary RSA key. The kid header is set to "wrong-key" so it does NOT
// match the issuer's JWKS entry; go-oidc will fail signature verification
// when it fetches the JWKS and finds no key with that ID.
func SignTokenWithKey(priv *rsa.PrivateKey, claims map[string]any) string {
	raw, err := json.Marshal(claims)
	if err != nil {
		panic("oidctest: json.Marshal claims: " + err.Error())
	}
	key := jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       priv,
	}
	opts := &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]interface{}{
			jose.HeaderKey("kid"): "wrong-key",
		},
	}
	signer, err := jose.NewSigner(key, opts)
	if err != nil {
		panic("oidctest: jose.NewSigner: " + err.Error())
	}
	sig, err := signer.Sign(raw)
	if err != nil {
		panic("oidctest: signer.Sign: " + err.Error())
	}
	tok, err := sig.CompactSerialize()
	if err != nil {
		panic("oidctest: CompactSerialize: " + err.Error())
	}
	return tok
}

// SignToken signs a JSON payload as a compact RS256 JWT.
func (fi *FakeIssuer) SignToken(claims map[string]any) string {
	raw, err := json.Marshal(claims)
	if err != nil {
		panic("oidctest: json.Marshal claims: " + err.Error())
	}
	key := jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       fi.priv,
	}
	opts := &jose.SignerOptions{
		ExtraHeaders: map[jose.HeaderKey]interface{}{
			jose.HeaderKey("kid"): fi.keyID,
		},
	}
	signer, err := jose.NewSigner(key, opts)
	if err != nil {
		panic("oidctest: jose.NewSigner: " + err.Error())
	}
	sig, err := signer.Sign(raw)
	if err != nil {
		panic("oidctest: signer.Sign: " + err.Error())
	}
	tok, err := sig.CompactSerialize()
	if err != nil {
		panic("oidctest: CompactSerialize: " + err.Error())
	}
	return tok
}

// tokenClaims merges standard claims with caller-supplied extras.
func (fi *FakeIssuer) tokenClaims(clientID, nonce string) map[string]any {
	fi.mu.RLock()
	fn := fi.ClaimsFunc
	defaults := fi.DefaultClaims
	fi.mu.RUnlock()

	now := time.Now()
	base := map[string]any{
		"iss":   fi.URL(),
		"aud":   clientID,
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"nonce": nonce,
		"sub":   "test-sub",
	}
	if fn != nil {
		extra := fn(clientID, nonce)
		for k, v := range extra {
			base[k] = v
		}
	} else {
		for k, v := range defaults {
			base[k] = v
		}
	}
	return base
}

// ServeHTTP implements http.Handler.
func (fi *FakeIssuer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		fi.serveDiscovery(w)
	case "/jwks":
		fi.serveJWKS(w)
	case "/token":
		fi.serveToken(w, r)
	default:
		http.Error(w, "oidctest: unknown path: "+r.URL.Path, http.StatusNotFound)
	}
}

func (fi *FakeIssuer) serveDiscovery(w http.ResponseWriter) {
	base := fi.URL()
	disc := map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/auth",
		"token_endpoint":                        base + "/token",
		"jwks_uri":                              base + "/jwks",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(disc); err != nil {
		panic("oidctest: serveDiscovery encode: " + err.Error())
	}
}

func (fi *FakeIssuer) serveJWKS(w http.ResponseWriter) {
	set := jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{
			{
				Key:       fi.priv.Public(),
				KeyID:     fi.keyID,
				Algorithm: "RS256",
				Use:       "sig",
			},
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(set); err != nil {
		panic("oidctest: serveJWKS encode: " + err.Error())
	}
}

// serveToken handles POST /token. It reads the nonce from the request
// form (passed as the code value during exchange), builds the token
// using the configured claims, and responds with the id_token.
func (fi *FakeIssuer) serveToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "parse form: "+err.Error(), http.StatusBadRequest)
		return
	}

	clientID := r.FormValue("client_id")
	// nonce is embedded in the code by Begin() as state
	nonce := r.FormValue("code")
	// read client_id from basic auth if not in form
	if clientID == "" {
		if id, _, ok := r.BasicAuth(); ok {
			clientID = id
		}
	}

	fi.mu.RLock()
	override := fi.TokenOverride
	suppress := fi.suppressIDToken
	refreshTok := fi.refreshToken
	fi.mu.RUnlock()

	if suppress {
		resp := map[string]any{
			"access_token": "x",
			"token_type":   "Bearer",
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			panic("oidctest: serveToken encode (suppress): " + err.Error())
		}
		return
	}

	var idToken string
	if override != nil {
		// TokenOverride is set: caller supplies the raw id_token (e.g. signed
		// by a key that is NOT in the JWKS, for signature-verification tests).
		idToken = override(clientID, nonce)
	} else {
		claims := fi.tokenClaims(clientID, nonce)
		idToken = fi.SignToken(claims)
	}

	resp := map[string]any{
		"access_token": "x",
		"token_type":   "Bearer",
		"id_token":     idToken,
		"expires_in":   strconv.Itoa(int(time.Hour.Seconds())),
	}
	if refreshTok != "" {
		resp["refresh_token"] = refreshTok
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		panic("oidctest: serveToken encode: " + err.Error())
	}
}

// SetClaims is a convenience method for tests: replaces DefaultClaims
// and clears ClaimsFunc. Thread-safe.
func (fi *FakeIssuer) SetClaims(claims map[string]any) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.DefaultClaims = claims
	fi.ClaimsFunc = nil
}

// SuppressIDToken causes the /token response to omit the id_token field.
// Thread-safe.
func (fi *FakeIssuer) SuppressIDToken() {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.suppressIDToken = true
}

// SetRefreshToken configures the /token response to include the given
// refresh_token value. Used by federation tests that assert a TokenSet
// is returned when offline_access is requested. Thread-safe.
func (fi *FakeIssuer) SetRefreshToken(rt string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.refreshToken = rt
}

// PastTime returns a Unix timestamp two hours in the past, for expiry tests.
func PastTime() int64 { return time.Now().Add(-2 * time.Hour).Unix() }

// FutureTime returns a Unix timestamp one hour in the future, for standard exp.
func FutureTime() int64 { return time.Now().Add(time.Hour).Unix() }
