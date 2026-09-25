// pkg/channels/channelkinds/slack/app_home_canonical_test.go
//
// Regression tests for resolveCanonicalForSlackUser's users.info fallback
// (app_home.go). The canonical SpiceDB subject it returns is what
// downstream authz (UserIdentity lookup, portal-link minting, session
// approver checks reached via other callers of this helper) keys on, so
// trusting a self-attested Slack profile email unconditionally would let
// any Slack account — a foreign-workspace guest, a restricted member, a
// deleted account — set their profile email to a real teammate's address
// and resolve to that teammate's canonical subject. emailTrusted() gates
// this; these tests pin that gate at the resolveCanonicalForSlackUser call
// site specifically (emailTrusted itself already has its own unit tests
// via the resolveIdentity/InteractionCallback paths).
package slack

import (
	"context"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestResolveCanonicalForSlackUser_ForeignWorkspaceEmailFallsBackToSynthetic
// is the FIX-1 regression test: a users.info lookup returns a profile email
// for a user whose TeamID does NOT match the bot's installed workspace (a
// Slack Connect / foreign-workspace account). That email must NOT be
// trusted for canonicalization — the fix must return the unforgeable
// synthetic slack:<team>:<user> subject instead of the email-derived one a
// spoofed profile email would otherwise produce.
func TestResolveCanonicalForSlackUser_ForeignWorkspaceEmailFallsBackToSynthetic(t *testing.T) {
	cli := fakeslack.New()
	cli.SeedUser(&slackapi.User{
		ID:      "U_SPOOF",
		TeamID:  "T_FOREIGN", // NOT the installed team ("T_HOME")
		Profile: slackapi.UserProfile{Email: "owner@example.com"},
	})
	l := &slackListener{api: cli, installedTeamID: "T_HOME"}

	got, err := l.resolveCanonicalForSlackUser(context.Background(), "U_SPOOF")
	require.NoError(t, err, "resolveCanonicalForSlackUser must succeed (degrades to synthetic, never errors here)")

	spoofedEmailCanonical, err := identity.FromExternal("slack", "T_HOME", "U_SPOOF", "owner@example.com").Subject()
	require.NoError(t, err)
	syntheticCanonical, err := identity.FromExternal("slack", "T_HOME", "U_SPOOF", "").AllowSynthetic().Subject()
	require.NoError(t, err)

	// identity boundary: asserts the string subject the resolver returns.
	assert.NotEqual(t, spoofedEmailCanonical.String(), got,
		"a foreign-workspace user's self-attested profile email must NOT yield the owner's email-derived canonical — that is the identity-spoofing escalation this test guards against")
	assert.Equal(t, syntheticCanonical.String(), got,
		"an untrusted email must fall back to the unforgeable synthetic slack:<team>:<user> subject")
}

// TestResolveCanonicalForSlackUser_RestrictedMemberFallsBackToSynthetic
// covers the other emailTrusted() gates on a same-workspace user: a guest
// (IsRestricted) member of the INSTALLED workspace is still low-trust and
// must not canonicalize by email even though TeamID matches.
func TestResolveCanonicalForSlackUser_RestrictedMemberFallsBackToSynthetic(t *testing.T) {
	cli := fakeslack.New()
	cli.SeedUser(&slackapi.User{
		ID:           "U_GUEST",
		TeamID:       "T_HOME",
		IsRestricted: true,
		Profile:      slackapi.UserProfile{Email: "owner@example.com"},
	})
	l := &slackListener{api: cli, installedTeamID: "T_HOME"}

	got, err := l.resolveCanonicalForSlackUser(context.Background(), "U_GUEST")
	require.NoError(t, err)

	spoofedEmailCanonical, err := identity.FromExternal("slack", "T_HOME", "U_GUEST", "owner@example.com").Subject()
	require.NoError(t, err)

	// identity boundary: asserts the string subject the resolver returns.
	assert.NotEqual(t, spoofedEmailCanonical.String(), got,
		"a restricted (guest) member's self-attested email must not be trusted for canonicalization")
}

// TestResolveCanonicalForSlackUser_TrustedMemberUsesEmail proves the happy
// path is unaffected by the fix: a full, non-restricted member of the
// installed workspace still canonicalizes by their profile email.
func TestResolveCanonicalForSlackUser_TrustedMemberUsesEmail(t *testing.T) {
	cli := fakeslack.New()
	cli.SeedUser(&slackapi.User{
		ID:      "U_REAL",
		TeamID:  "T_HOME",
		Profile: slackapi.UserProfile{Email: "real@example.com"},
	})
	l := &slackListener{api: cli, installedTeamID: "T_HOME"}

	got, err := l.resolveCanonicalForSlackUser(context.Background(), "U_REAL")
	require.NoError(t, err)

	wantCanonical, err := identity.FromExternal("slack", "T_HOME", "U_REAL", "real@example.com").Subject()
	require.NoError(t, err)

	// identity boundary: asserts the string subject the resolver returns.
	assert.Equal(t, wantCanonical.String(), got,
		"a trusted (installed-workspace, non-restricted) member's email must still resolve to the email-derived canonical")
}
