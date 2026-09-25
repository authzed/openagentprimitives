package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// authorizeEndpoint is the Sign-in-with-Slack authorize URL.
const authorizeEndpoint = "https://slack.com/openid/connect/authorize"

// tokenEndpoint is the OIDC token-exchange URL. Test seam: overridden in
// oidc_test.go via httptest.NewServer.
var tokenEndpoint = "https://slack.com/api/openid.connect.token"

// slackIssuer is the expected `iss` claim on Slack-issued id_tokens.
const slackIssuer = "https://slack.com"

// slackTeamClaim is the Slack-prefixed id_token claim carrying the workspace
// (team) ID the user authenticated against. We constrain this to the
// installed workspace; without that check, any Slack user from any workspace
// whose verified email matches a victim's collapses to the same canonical
// SpiceDB subject (see identity.Principal) and impersonates them.
const slackTeamClaim = "https://slack.com/team_id"

// slackAuth is the channelkinds.WebAuthenticator for "Sign in with Slack".
type slackAuth struct {
	clientID, clientSecret, externalBaseURL string
	installedTeamID                         string
	httpClient                              *http.Client
}

// newSlackAuth constructs a slackAuth. Used from kind.go's WebAuthenticator
// method and from tests. A nil hc produces a default 30-second-timeout client.
func newSlackAuth(deps channelkinds.WebAuthDeps, hc *http.Client) *slackAuth {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &slackAuth{
		clientID:        deps.ClientID,
		clientSecret:    deps.ClientSecret,
		externalBaseURL: deps.ExternalBaseURL,
		installedTeamID: deps.InstalledTeamID,
		httpClient:      hc,
	}
}

func (a *slackAuth) redirectURI() string {
	return strings.TrimRight(a.externalBaseURL, "/") + "/oidc/callback/slack"
}

// Begin returns the Slack OIDC authorize URL the caller should redirect the
// user to. Returns ErrAuthenticatorUnavailable when any of ClientID,
// ClientSecret, ExternalBaseURL, or InstalledTeamID is empty — without a
// known installed workspace we have no anchor against which to gate the
// id_token's `https://slack.com/team_id` claim, and Complete would
// fail closed anyway.
func (a *slackAuth) Begin(_ context.Context, state string) (string, error) {
	if a.clientID == "" || a.clientSecret == "" || a.externalBaseURL == "" || a.installedTeamID == "" {
		return "", fmt.Errorf("%w: slack OIDC needs ClientID + ClientSecret + ExternalBaseURL + InstalledTeamID", channelkinds.ErrAuthenticatorUnavailable)
	}
	q := url.Values{
		"response_type": {"code"},
		"client_id":     {a.clientID},
		"scope":         {"openid email profile"},
		"redirect_uri":  {a.redirectURI()},
		"state":         {state},
		// Pin to the installed workspace so Slack rejects sign-in by users
		// from other workspaces before we ever see the id_token. The
		// server-side claim check in Complete is the load-bearing
		// enforcement; this is defense-in-depth + a better UX (Slack
		// short-circuits the wrong-workspace case at the picker).
		"team": {a.installedTeamID},
	}
	// url.Values.Encode encodes spaces as "+", so "openid email profile"
	// becomes "openid+email+profile" as required by the Slack OIDC spec.
	return authorizeEndpoint + "?" + q.Encode(), nil
}

// Complete exchanges the authorization code for an OIDC id_token from Slack,
// validates iss / aud / email_verified / team_id, decodes the email claim,
// builds a verified-email Principal, and returns "user:<canonical>".
//
// The id_token signature is intentionally not verified: per OIDC Core §3.1.3.5
// the id_token's authenticity is established by transport security plus the
// confidential-client authentication of the token request (we POST our
// client_secret over HTTPS to slack.com/api/openid.connect.token, and Slack
// returns the id_token directly in that response — there is no untrusted
// hop). The claim checks below are about WHICH user the token represents,
// not whether it is authentic.
func (a *slackAuth) Complete(ctx context.Context, cb channelkinds.CallbackParams) (string, error) {
	if a.clientID == "" || a.clientSecret == "" {
		return "", fmt.Errorf("%w: slack OIDC needs ClientID + ClientSecret", channelkinds.ErrAuthenticatorUnavailable)
	}
	if a.installedTeamID == "" {
		// Fail closed. Without a known installed workspace we cannot enforce
		// the team_id claim, and accepting any id_token at this point would
		// let any Slack user impersonate any other with a matching email.
		return "", fmt.Errorf("%w: slack OIDC needs InstalledTeamID to gate cross-workspace impersonation", channelkinds.ErrAuthenticatorUnavailable)
	}
	formBody := url.Values{
		"code":          {cb.Code},
		"client_id":     {a.clientID},
		"client_secret": {a.clientSecret},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {a.redirectURI()},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(formBody))
	if err != nil {
		return "", fmt.Errorf("slack oidc: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack oidc: token endpoint: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("slack oidc: read token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("slack oidc: token endpoint returned %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var envelope struct {
		// OK false means the call failed even though HTTP returned 2xx.
		OK bool `json:"ok"`
		// Error is Slack's machine-readable failure code; empty when OK.
		Error string `json:"error"`
		// IDToken is the signed JWT carrying the user's claims.
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return "", fmt.Errorf("slack oidc: parse token response: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if !envelope.OK {
		return "", fmt.Errorf("slack oidc: token endpoint error: %s", envelope.Error)
	}
	if envelope.IDToken == "" {
		return "", fmt.Errorf("slack oidc: token response missing id_token")
	}

	claims, err := decodeIDTokenClaims(envelope.IDToken)
	if err != nil {
		return "", err
	}

	if iss, _ := claims["iss"].(string); iss != slackIssuer {
		return "", fmt.Errorf("slack oidc: id_token iss %q != %q", iss, slackIssuer)
	}
	if !audienceMatches(claims["aud"], a.clientID) {
		return "", fmt.Errorf("slack oidc: id_token aud does not include client_id")
	}
	// Slack's id_token carries `https://slack.com/team_id` for the workspace
	// the user authenticated against. Reject anything that didn't sign in
	// to OUR installed workspace; without this, Slack's global authorize
	// endpoint accepts any user from any workspace and the canonical
	// derivation collapses different humans to the same SpiceDB subject.
	team, _ := claims[slackTeamClaim].(string)
	if team == "" {
		return "", fmt.Errorf("slack oidc: id_token missing %q claim", slackTeamClaim)
	}
	if team != a.installedTeamID {
		return "", fmt.Errorf("slack oidc: id_token team %q != installed team", team)
	}
	verified, _ := claims["email_verified"].(bool)
	if !verified {
		return "", fmt.Errorf("slack oidc: id_token email_verified is not true")
	}

	email, _ := claims["email"].(string)
	if email == "" {
		return "", fmt.Errorf("slack oidc: id_token missing email claim")
	}

	// Build a verified-email Principal; identity.VerifiedEmail lowercases
	// the email and marks it proven by this completed OIDC flow. The email is
	// non-empty (checked above), so Subject never hits the synthetic guard.
	subj, err := identity.VerifiedEmail(identity.Email(email), "").Subject()
	// identity boundary: the OIDC user resolver returns a (string, error) subject; serialized here.
	return subj.String(), err
}

// audienceMatches reports whether the id_token's `aud` claim — which OIDC
// allows as either a string OR a JSON array of strings — names our client.
func audienceMatches(aud any, clientID string) bool {
	if clientID == "" {
		return false
	}
	switch v := aud.(type) {
	case string:
		return v == clientID
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && s == clientID {
				return true
			}
		}
	}
	return false
}

// decodeIDTokenClaims splits a JWT (header.payload.signature), base64url-
// decodes the payload, and returns it as a map. Does NOT verify the
// signature — the id_token's authenticity is established by the token
// endpoint having authenticated the request with client_secret.
func decodeIDTokenClaims(idToken string) (map[string]any, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("slack oidc: id_token has %d parts, want 3", len(parts))
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("slack oidc: id_token claims base64 decode: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(body, &claims); err != nil {
		return nil, fmt.Errorf("slack oidc: id_token claims JSON parse: %w", err)
	}
	return claims, nil
}

// truncate returns at most n bytes of s, appending "..." if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
