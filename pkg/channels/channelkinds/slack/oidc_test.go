package slack

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// makeIDToken builds an unsigned JWT with the given claims for tests.
// Header + payload base64url + signature placeholder ".sig" — the impl
// doesn't verify, so the signature placeholder is fine.
func makeIDToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	headerB64 := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body, err := json.Marshal(claims)
	require.NoError(t, err)
	bodyB64 := base64.RawURLEncoding.EncodeToString(body)
	return headerB64 + "." + bodyB64 + ".sig"
}

// validClaims returns the minimal claim set that the hardened slackAuth
// will accept: email + email_verified, the Slack-prefixed team_id matching
// the installed workspace, plus iss/aud aligned to the test fixtures.
func validClaims(email, teamID, clientID string) map[string]any {
	return map[string]any{
		"iss":                       "https://slack.com",
		"aud":                       clientID,
		"email":                     email,
		"email_verified":            true,
		"https://slack.com/team_id": teamID,
	}
}

// installedTeamFixture is the installed workspace ID used by tests below.
const installedTeamFixture = "T_HOME"

func TestBegin_AuthorizeURL(t *testing.T) {
	deps := channelkinds.WebAuthDeps{
		ClientID:        "abc.123",
		ClientSecret:    "secret",
		ExternalBaseURL: "https://example.org",
		InstalledTeamID: installedTeamFixture,
	}
	auth := newSlackAuth(deps, http.DefaultClient)
	url, err := auth.Begin(context.Background(), "csrf-token")
	require.NoError(t, err)
	assert.Contains(t, url, "https://slack.com/openid/connect/authorize")
	assert.Contains(t, url, "client_id=abc.123")
	assert.Contains(t, url, "state=csrf-token")
	assert.Contains(t, url, "scope=openid+email+profile")
	assert.Contains(t, url, "redirect_uri=https%3A%2F%2Fexample.org%2Foidc%2Fcallback%2Fslack")
	// Defense-in-depth: pin authorize to the installed workspace so Slack
	// itself rejects sign-in from foreign workspaces before we even see the
	// id_token. Cooperates with the server-side team_id claim check.
	assert.Contains(t, url, "team=T_HOME")
}

func TestBegin_UnconfiguredReturnsSentinel(t *testing.T) {
	auth := newSlackAuth(channelkinds.WebAuthDeps{}, http.DefaultClient)
	_, err := auth.Begin(context.Background(), "s")
	require.ErrorIs(t, err, channelkinds.ErrAuthenticatorUnavailable)
}

func TestBegin_MissingInstalledTeamIDReturnsSentinel(t *testing.T) {
	// Without InstalledTeamID we cannot pin the authorize URL nor verify
	// the id_token's team_id — fail closed.
	auth := newSlackAuth(channelkinds.WebAuthDeps{
		ClientID: "id", ClientSecret: "sec", ExternalBaseURL: "https://e.org",
	}, http.DefaultClient)
	_, err := auth.Begin(context.Background(), "s")
	require.ErrorIs(t, err, channelkinds.ErrAuthenticatorUnavailable)
}

// completeTestServer wires a token-endpoint stub returning idToken.
// Returns the configured slackAuth and a complete-call closure.
func completeTestServer(t *testing.T, idToken string, deps channelkinds.WebAuthDeps) (*slackAuth, func() (string, error)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"id_token":%q}`, idToken)
	}))
	t.Cleanup(srv.Close)
	prev := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() { tokenEndpoint = prev })

	auth := newSlackAuth(deps, http.DefaultClient)
	return auth, func() (string, error) {
		return auth.Complete(context.Background(), channelkinds.CallbackParams{Code: "c", State: "s"})
	}
}

// standardDeps returns the test-fixture deps with InstalledTeamID populated.
func standardDeps() channelkinds.WebAuthDeps {
	return channelkinds.WebAuthDeps{
		ClientID: "id", ClientSecret: "sec",
		ExternalBaseURL: "https://e.org",
		InstalledTeamID: installedTeamFixture,
	}
}

func TestComplete_HappyPath(t *testing.T) {
	idToken := makeIDToken(t, validClaims("Alice.Example@Example.COM", installedTeamFixture, "id"))
	_, complete := completeTestServer(t, idToken, standardDeps())
	canonical, err := complete()
	require.NoError(t, err)
	// The canonical for "alice.example@example.com" via the project's
	// Canonicalize function — assert the prefix and that it's lower-cased.
	assert.True(t, strings.HasPrefix(canonical, "user:"), "expected user: prefix, got %q", canonical)
}

func TestComplete_RejectsUnverifiedEmail(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	claims["email_verified"] = false
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "email_verified")
}

func TestComplete_RejectsMissingEmailVerified(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	delete(claims, "email_verified")
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "email_verified")
}

func TestComplete_RejectsForeignTeam(t *testing.T) {
	// Mallory's id_token claims a different workspace than the installed one.
	idToken := makeIDToken(t, validClaims("alice@example.com", "T_MALLORY", "id"))
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team")
}

func TestComplete_RejectsMissingTeamClaim(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	delete(claims, "https://slack.com/team_id")
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "team")
}

func TestComplete_RejectsBadIssuer(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	claims["iss"] = "https://evil.example.com"
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "iss")
}

func TestComplete_RejectsBadAudienceString(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	claims["aud"] = "some-other-client"
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

func TestComplete_AcceptsAudienceArrayContainingClient(t *testing.T) {
	// OIDC spec allows `aud` to be a string OR a JSON array of strings.
	// Accept when our client_id is in the array.
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	claims["aud"] = []any{"some-other-client", "id"}
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.NoError(t, err)
}

func TestComplete_RejectsAudienceArrayNotContainingClient(t *testing.T) {
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	claims["aud"] = []any{"some-other-client", "yet-another"}
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "aud")
}

func TestComplete_FailsClosedWhenInstalledTeamIDMissing(t *testing.T) {
	// Deps without InstalledTeamID must reject every id_token, even one
	// otherwise well-formed — there's no installed workspace to compare to.
	idToken := makeIDToken(t, validClaims("alice@example.com", installedTeamFixture, "id"))
	deps := channelkinds.WebAuthDeps{
		ClientID: "id", ClientSecret: "sec", ExternalBaseURL: "https://e.org",
		// InstalledTeamID intentionally unset.
	}
	_, complete := completeTestServer(t, idToken, deps)
	_, err := complete()
	require.ErrorIs(t, err, channelkinds.ErrAuthenticatorUnavailable)
}

func TestComplete_TokenEndpoint5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	prev := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() { tokenEndpoint = prev })

	auth := newSlackAuth(standardDeps(), http.DefaultClient)
	_, err := auth.Complete(context.Background(), channelkinds.CallbackParams{Code: "c", State: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500", "expected status code in error message, got %q", err.Error())
}

func TestComplete_SlackOKFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":false,"error":"invalid_code"}`)
	}))
	t.Cleanup(srv.Close)
	prev := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() { tokenEndpoint = prev })

	auth := newSlackAuth(standardDeps(), http.DefaultClient)
	_, err := auth.Complete(context.Background(), channelkinds.CallbackParams{Code: "c", State: "s"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_code")
}

func TestComplete_MissingEmailClaim(t *testing.T) {
	// Otherwise-valid id_token but no email claim.
	claims := validClaims("alice@example.com", installedTeamFixture, "id")
	delete(claims, "email")
	idToken := makeIDToken(t, claims)
	_, complete := completeTestServer(t, idToken, standardDeps())
	_, err := complete()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "email")
}

func TestComplete_MalformedIDToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"id_token":"not-a-jwt"}`)
	}))
	t.Cleanup(srv.Close)
	prev := tokenEndpoint
	tokenEndpoint = srv.URL
	t.Cleanup(func() { tokenEndpoint = prev })

	auth := newSlackAuth(standardDeps(), http.DefaultClient)
	_, err := auth.Complete(context.Background(), channelkinds.CallbackParams{Code: "c", State: "s"})
	require.Error(t, err)
}
