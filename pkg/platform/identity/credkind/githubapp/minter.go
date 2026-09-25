// Package githubapp mints GitHub App installation access tokens.
//
// Two legs: sign a short-lived RS256 JWT with the App's private key, then
// exchange it for an installation token. The JWT is capped at 10 minutes
// because that is GitHub's maximum; the installation token GitHub returns
// lives ~1h and always carries an expires_at, which is what lets the broker
// cache it safely. A minted token's short life is the only thing bounding a
// revoked token's usefulness, so this package fails closed rather than ever
// return a token with a zero expiry.
package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
)

// Minter mints GitHub App installation access tokens. Implementations MUST
// NOT log key or token bytes, and MUST name the failing leg (JWT signing vs.
// the access_tokens exchange) in returned errors.
type Minter interface {
	Mint(ctx context.Context, req MintRequest) (MintedToken, error)
}

// MintRequest names the App, its private key, and the installation to mint a
// token for.
type MintRequest struct {
	// AppID is the GitHub App's numeric id, used as the JWT issuer.
	AppID string
	// PrivateKeyPEM is the App's RS256 private key in PEM form. Never logged.
	PrivateKeyPEM []byte
	// InstallationID is the installation the minted token is scoped to.
	InstallationID string
}

// MintedToken is a GitHub App installation access token plus its expiry. The
// token is memory-only; callers MUST NOT persist it beyond the cache the
// broker keeps until ExpiresAt.
type MintedToken struct {
	AccessToken sensitive.SensitiveValue
	ExpiresAt   time.Time
}

// defaultBaseURL is GitHub's REST API host. Tests override it via
// WithBaseURL to point at an httptest server.
const defaultBaseURL = "https://api.github.com"

// jwtLifetime is under GitHub's 10-minute ceiling, leaving room for clock
// skew between this process and GitHub's.
const jwtLifetime = 9 * time.Minute

// jwtClockSkewTolerance backdates the JWT's issued-at so a modest clock skew
// between this process and GitHub's doesn't cause a not-yet-valid rejection.
const jwtClockSkewTolerance = 30 * time.Second

// maxResponseBodyBytes bounds how much of the access_tokens response this
// package will read, so a misbehaving or malicious server can't force
// unbounded memory growth.
const maxResponseBodyBytes = 1 << 20 // 1 MiB

// HTTPMinter mints installation tokens by calling the real GitHub REST API
// (or a stand-in server pointed at via WithBaseURL).
type HTTPMinter struct {
	baseURL string
	client  *http.Client
	now     func() time.Time
}

var _ Minter = (*HTTPMinter)(nil)

// Option configures an HTTPMinter.
type Option func(*HTTPMinter)

// WithBaseURL overrides the API host the minter talks to. Tests use this to
// point at an httptest server instead of api.github.com.
func WithBaseURL(u string) Option {
	return func(m *HTTPMinter) { m.baseURL = u }
}

// WithHTTPClient overrides the HTTP client used for the access_tokens
// exchange.
func WithHTTPClient(c *http.Client) Option {
	return func(m *HTTPMinter) { m.client = c }
}

// WithClock overrides the minter's notion of "now", used to stamp the JWT's
// issued-at/expiry. Tests use this for deterministic assertions.
func WithClock(f func() time.Time) Option {
	return func(m *HTTPMinter) { m.now = f }
}

// NewHTTPMinter builds an HTTPMinter against the real GitHub API, or a
// stand-in configured via options.
func NewHTTPMinter(opts ...Option) *HTTPMinter {
	m := &HTTPMinter{
		baseURL: defaultBaseURL,
		client:  &http.Client{Timeout: 20 * time.Second},
		now:     time.Now,
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Mint signs an App JWT and exchanges it for an installation access token.
func (m *HTTPMinter) Mint(ctx context.Context, req MintRequest) (MintedToken, error) {
	signed, err := m.signAppJWT(req)
	if err != nil {
		return MintedToken{}, err
	}
	return m.exchangeForInstallationToken(ctx, req, signed)
}

// signAppJWT builds the leg-1 App JWT, stamped with this minter's clock.
func (m *HTTPMinter) signAppJWT(req MintRequest) (string, error) {
	return SignAppJWT(req.AppID, req.PrivateKeyPEM, m.now())
}

// SignAppJWT signs a short-lived RS256 JWT authenticating as the GitHub App
// itself — App-level auth, as distinct from an installation access token.
// Exported so a second caller needing the same App-level auth (the
// appprovision package's read of an already-provisioned App's config, which
// GitHub authenticates the same way) reuses this instead of hand-rolling the
// claim shape and lifetime a second time; Mint's leg-1 (above) is now just
// this function plus this minter's own clock.
//
// It never wraps the underlying jwt package's parse error: that error can
// echo back the (invalid) input it was given, and privateKeyPEM must never
// reach an error message.
func SignAppJWT(appID string, privateKeyPEM []byte, now time.Time) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM(privateKeyPEM)
	if err != nil {
		return "", fmt.Errorf("githubapp: app %s: sign JWT: private key is not a valid RSA PEM", appID)
	}

	claims := jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-jwtClockSkewTolerance)),
		ExpiresAt: jwt.NewNumericDate(now.Add(jwtLifetime)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		return "", fmt.Errorf("githubapp: app %s: sign JWT: %w", appID, err)
	}
	return signed, nil
}

// exchangeForInstallationToken is leg 2: POST the signed App JWT to
// /app/installations/{id}/access_tokens and parse the resulting installation
// token. It fails closed if the response is missing a token or an expiry —
// an unbounded token is exactly the failure this package guards against,
// since the broker caches a minted credential until its expiry.
func (m *HTTPMinter) exchangeForInstallationToken(ctx context.Context, req MintRequest, appJWT string) (MintedToken, error) {
	url := strings.TrimRight(m.baseURL, "/") + "/app/installations/" + req.InstallationID + "/access_tokens"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return MintedToken{}, fmt.Errorf("githubapp: build access_tokens request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+appJWT)
	httpReq.Header.Set("Accept", "application/vnd.github+json")

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return MintedToken{}, fmt.Errorf("githubapp: access_tokens exchange: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes))
	if err != nil {
		return MintedToken{}, fmt.Errorf("githubapp: access_tokens exchange: read response: %w", err)
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Deliberately EXCLUDES the response body, matching this package's
		// sibling on the same GitHub surface (channelkinds/github/appprovision's
		// client.go) for the same two reasons, one of which bites harder here.
		//
		// Size: this error is returned through Resolve to the operator, which
		// surfaces it on a status condition. metav1.Condition.Message is capped
		// at 32768 characters by the CRD schema, and the body read above is
		// bounded only by maxResponseBodyBytes (1 MiB). An upstream returning a
		// large body — an HTML error page from a proxy in front of the API is
		// the ordinary way this happens, not a hostile one — makes the STATUS
		// WRITE fail validation. The reconcile then errors, requeues, mints
		// again, and fails to write again: a hot loop whose visible symptom is
		// not the upstream error at all.
		//
		// Secrecy: GitHub's error body is not secret today, but nothing
		// guarantees it stays that way, and this path handles App credentials.
		//
		// The status code carries the diagnosis an operator needs — 401 is a
		// bad App JWT or clock skew, 404 is a wrong installation id, 403 is a
		// suspended installation. (pkg/platform/identity/refresh truncates its
		// body to 200 instead; that is a different upstream and a different
		// error surface, and either bound is fine — an UNBOUNDED one is not.)
		return MintedToken{}, fmt.Errorf("githubapp: access_tokens exchange for installation %s: status %d",
			req.InstallationID, resp.StatusCode)
	}

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return MintedToken{}, fmt.Errorf("githubapp: access_tokens exchange: decode response: %w", err)
	}
	if out.Token == "" || out.ExpiresAt.IsZero() {
		return MintedToken{}, fmt.Errorf(
			"githubapp: access_tokens exchange for installation %s: response missing token or expires_at",
			req.InstallationID)
	}

	return MintedToken{
		AccessToken: sensitive.NewSensitiveValue([]byte(out.Token)),
		ExpiresAt:   out.ExpiresAt,
	}, nil
}
