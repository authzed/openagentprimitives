package subjectresolve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// userNamed builds the UserIdentity channelsd records for a full member of the
// installed workspace: the email-canonical subject of addr, spec.displayName
// set from the Slack display name, and a Slack channel identity carrying
// slackDisplay/slackReal alongside the email the channel vouched for.
func userNamed(t *testing.T, addr, slackDisplay, slackReal string) spiceboxv1alpha1.UserIdentity {
	t.Helper()
	return spiceboxv1alpha1.UserIdentity{
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:" + mustEmailSubject(t, addr),
			DisplayName: slackDisplay,
		},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{{
			Kind: "slack", Domain: "T1", ExternalID: "U-" + addr,
			DisplayName: slackDisplay, RealName: slackReal, Email: addr,
		}}},
	}
}

// unvouchedNamed builds the UserIdentity channelsd records for a sender the
// channel did not vouch for — a guest, shared-channel stranger or
// foreign-workspace user: a team-scoped synthetic subject, and a channel
// identity with the self-set names but no email.
func unvouchedNamed(team, userID, slackReal string) spiceboxv1alpha1.UserIdentity {
	return spiceboxv1alpha1.UserIdentity{
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:synthetic-" + team + "-" + userID,
			DisplayName: slackReal,
		},
		Status: spiceboxv1alpha1.UserIdentityStatus{ChannelIdentities: []spiceboxv1alpha1.ChannelIdentity{{
			Kind: "slack", Domain: team, ExternalID: userID,
			DisplayName: slackReal, RealName: slackReal,
		}}},
	}
}

func fakeUsers(users ...spiceboxv1alpha1.UserIdentity) func(context.Context) ([]spiceboxv1alpha1.UserIdentity, error) {
	return func(context.Context) ([]spiceboxv1alpha1.UserIdentity, error) { return users, nil }
}

func TestResolve_Name(t *testing.T) {
	directory := fakeUsers(
		// Display name is a handle; only the Slack real name carries the full name.
		userNamed(t, "pat@example.com", "pat", "Pat Quinlan"),
		userNamed(t, "jose@example.com", "squid", "José Núñez Peña"),
		userNamed(t, "kim.lee@example.com", "Kim Lee", ""),
		// Two different vouched people recorded under one full name.
		userNamed(t, "sam.ortiz@example.com", "Sam Ortiz", ""),
		userNamed(t, "sortiz@example.com", "sortiz", "Sam Ortiz"),
		// A guest who set their name to a member's: neither a match nor an
		// ambiguity vote, so Pat still resolves.
		unvouchedNamed("T1", "UGUEST", "Pat Quinlan"),
		// A guest, and a foreign-workspace user, claiming a name no vouched
		// member carries: a self-set name alone must never resolve.
		unvouchedNamed("T1", "UGUEST2", "Dana Whitfield"),
		unvouchedNamed("TFOREIGN", "UFOREIGN", "Ari Castellanos"),
		// Only spec.displayName carries the name, with no vouched channel
		// identity behind it.
		spiceboxv1alpha1.UserIdentity{Spec: spiceboxv1alpha1.UserIdentitySpec{Subject: "user:" + mustEmailSubject(t, "lee@example.com"), DisplayName: "Lee Chandra"}},
	)

	cases := []struct {
		name        string
		ref         string
		wantSubject string
	}{
		{name: "full name on the Slack real name resolves", ref: "name:Pat Quinlan", wantSubject: mustEmailSubject(t, "pat@example.com")},
		{name: "case and spacing are ignored", ref: "name:  pat   QUINLAN ", wantSubject: mustEmailSubject(t, "pat@example.com")},
		{name: "accents are ignored", ref: "name:Jose Nunez Pena", wantSubject: mustEmailSubject(t, "jose@example.com")},
		{name: "full name on a vouched Slack display name resolves", ref: "name:Kim Lee", wantSubject: mustEmailSubject(t, "kim.lee@example.com")},
		{name: "two vouched users share the name: unresolved", ref: "name:Sam Ortiz"},
		{name: "name only an unvouched guest carries: unresolved", ref: "name:Dana Whitfield"},
		{name: "name only a foreign-workspace user carries: unresolved", ref: "name:Ari Castellanos"},
		{name: "name only on spec.displayName: unresolved", ref: "name:Lee Chandra"},
		{name: "nobody has the name: unresolved", ref: "name:Robin Hale"},
		{name: "single word is not a full name: unresolved even when one user matches", ref: "name:pat"},
		{name: "empty name: unresolved", ref: "name:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Resolve(t.Context(), tc.ref, Env{UserIdentities: directory})
			require.NoError(t, err)
			assert.Equal(t, tc.wantSubject, res.Subject)
			assert.False(t, res.SubjectProven, "a name match never proves the reference")
			if tc.wantSubject == "" {
				assert.NotEmpty(t, res.Reason)
			} else {
				assert.Empty(t, res.Reason)
			}
		})
	}
}

func TestResolve_NameSameUserUnderSeveralNamesCountsOnce(t *testing.T) {
	ui := userNamed(t, "kim.lee@example.com", "Kim Lee", "Kim Lee")
	res, err := Resolve(t.Context(), "name:Kim Lee", Env{UserIdentities: fakeUsers(ui)})
	require.NoError(t, err)
	assert.Equal(t, mustEmailSubject(t, "kim.lee@example.com"), res.Subject)
}

func TestResolve_NameWithoutDirectoryIsUnresolved(t *testing.T) {
	res, err := Resolve(t.Context(), "name:Kim Lee", Env{})
	require.NoError(t, err)
	assert.Empty(t, res.Subject)
	assert.Equal(t, noUserDirectoryReason, res.Reason)
}

func TestResolve_NameDirectoryErrorIsReturned(t *testing.T) {
	boom := errors.New("list failed")
	_, err := Resolve(t.Context(), "name:Kim Lee", Env{
		UserIdentities: func(context.Context) ([]spiceboxv1alpha1.UserIdentity, error) { return nil, boom },
	})
	assert.ErrorIs(t, err, boom)
}

func TestResolve_NameContainingAtIsNotClaimedAsEmail(t *testing.T) {
	res, err := Resolve(t.Context(), "name:Kim @ Lee", Env{UserIdentities: fakeUsers()})
	require.NoError(t, err)
	assert.Empty(t, res.Subject)
	assert.Contains(t, res.Reason, "recorded under the name")
}
