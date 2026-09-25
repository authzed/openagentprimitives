// Package idjag is the vendor-neutral ID-JAG (Identity Assertion JWT
// Authorization Grant, draft-ietf-oauth-identity-assertion-authz-grant)
// federation.Minter. Leg 1: RFC 8693 token-exchange at the IdP yields an
// ID-JAG audienced to the resource. Leg 2: present the ID-JAG as a JWT
// authorization grant at the resource's authorization server (discovered
// via RFC 9728 → 8414) for the upstream access token. Both legs go through
// the SSRF-guarded client the caller supplies.
package idjag

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/platform/identity/federation"
	"github.com/authzed/openagentprimitives/pkg/platform/identity/sensitive"
	"github.com/authzed/openagentprimitives/pkg/tools/mcp/oauth"
)

const (
	grantTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	grantJWTBearer     = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	tokenTypeIDToken   = "urn:ietf:params:oauth:token-type:id_token"
	tokenTypeIDJAG     = "urn:ietf:params:oauth:token-type:id-jag"
)

// DefaultTokenTTL bounds the assumed lifetime of an upstream access token whose
// authorization server omitted expires_in (RFC 6749 §5.1 makes it OPTIONAL).
//
// This is a security bound, not a convenience default. The broker caches a
// resolved credential until its expiry and reads a ZERO expiry as "never
// expires" (pkg/platform/identity/broker/inproc/broker.go). Stamping zero here would pin
// a federated token — whose entire purpose is a short life — in the broker cache
// for the lifetime of the process, so a mid-session revocation could never take
// effect. We would rather re-mint every DefaultTokenTTL than serve a token we
// cannot reason about.
const DefaultTokenTTL = 5 * time.Minute

// Minter implements federation.Minter against the ID-JAG draft.
type Minter struct{ hc *http.Client }

// New returns a Minter using hc for both legs. Pass safehttp.Client().
func New(hc *http.Client) *Minter { return &Minter{hc: hc} }

var _ federation.Minter = (*Minter)(nil)

func (m *Minter) Mint(ctx context.Context, req federation.MintRequest) (federation.MintedToken, error) {
	idjag, err := m.exchangeForIDJAG(ctx, req)
	if err != nil {
		return federation.MintedToken{}, fmt.Errorf("idjag: leg 1 token-exchange: %w", err)
	}
	tok, exp, err := m.grantAtResource(ctx, req, idjag)
	if err != nil {
		return federation.MintedToken{}, fmt.Errorf("idjag: leg 2 resource grant: %w", err)
	}
	return federation.MintedToken{AccessToken: sensitive.NewSensitiveValue([]byte(tok)), ExpiresAt: exp}, nil
}

func (m *Minter) exchangeForIDJAG(ctx context.Context, req federation.MintRequest) (string, error) {
	form := url.Values{}
	form.Set("grant_type", grantTokenExchange)
	form.Set("subject_token", string(req.Subject.Token.UnderlyingValue()))
	form.Set("subject_token_type", tokenTypeIDToken)
	form.Set("requested_token_type", tokenTypeIDJAG)
	form.Set("resource", req.Resource)
	form.Set("client_id", req.Subject.ClientID)
	if req.Subject.ClientSecret != "" {
		form.Set("client_secret", req.Subject.ClientSecret)
	}
	tok, _, err := m.postToken(ctx, req.Subject.IdPTokenEndpoint, form)
	return tok, err
}

func (m *Minter) grantAtResource(ctx context.Context, req federation.MintRequest, idjag string) (string, time.Time, error) {
	meta, err := oauth.Discover(ctx, m.hc, req.ResourceServerURL)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("discover resource AS: %w", err)
	}
	form := url.Values{}
	form.Set("grant_type", grantJWTBearer)
	form.Set("assertion", idjag)
	if len(req.Scopes) > 0 {
		form.Set("scope", strings.Join(req.Scopes, " "))
	}
	tok, expiresIn, err := m.postToken(ctx, meta.TokenEndpoint, form)
	if err != nil {
		return "", time.Time{}, err
	}
	// A missing or non-positive expires_in must NOT become a zero expiry: the
	// broker reads zero as "never expires" and would cache this token for the
	// life of the process. Fall back to the bounded DefaultTokenTTL instead.
	ttl := time.Duration(expiresIn) * time.Second
	if expiresIn <= 0 {
		ttl = DefaultTokenTTL
	}
	return tok, time.Now().Add(ttl), nil
}

// postToken POSTs a form to a token endpoint and returns access_token +
// expires_in. Non-2xx and unparseable bodies are errors (no token bytes
// in the message).
func (m *Minter) postToken(ctx context.Context, endpoint string, form url.Values) (string, int, error) {
	if endpoint == "" {
		return "", 0, fmt.Errorf("empty token endpoint")
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.hc.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("read token response body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", 0, fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
	}
	var out struct {
		// SECRET: the ID-JAG assertion (leg 1) or the upstream access token
		// (leg 2). Empty is a protocol error and is refused below.
		AccessToken string `json:"access_token"`
		// Lifetime in seconds; 0 means the AS omitted it (RFC 6749 §5.1 allows
		// that) and the caller substitutes DefaultTokenTTL rather than "never".
		ExpiresIn int `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", 0, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", 0, fmt.Errorf("%s response missing access_token", endpoint)
	}
	return out.AccessToken, out.ExpiresIn, nil
}
