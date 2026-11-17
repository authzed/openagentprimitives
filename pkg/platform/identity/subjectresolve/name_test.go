package subjectresolve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
)

// userNamed builds a UserIdentity for the email-canonical subject of addr,
// recording display as its spec.displayName and, when non-empty, a Slack
// channel identity carrying slackDisplay/slackReal.
func userNamed(t *testing.T, addr, display, slackDisplay, slackReal string) spiceboxv1alpha1.UserIdentity {
	t.Helper()
	ui := spiceboxv1alpha1.UserIdentity{
		Spec: spiceboxv1alpha1.UserIdentitySpec{
			Subject:     "user:" + mustEmailSubject(t, addr),
			DisplayName: display,
		},
	}
	if slackDisplay != "" || slackReal != "" {
		ui.Status.ChannelIdentities = []spiceboxv1alpha1.ChannelIdentity{{
			Kind: "slack", Domain: "T1", ExternalID: "U-" + addr,
			DisplayName: slackDisplay, RealName: slackReal, Email: addr,
		}}
	}
	return ui
}

func fakeUsers(users ...spiceboxv1alpha1.UserIdentity) func(context.Context) ([]spiceboxv1alpha1.UserIdentity, error) {
	return func(context.Context) ([]spiceboxv1alpha1.UserIdentity, error) { return users, nil }
}

func TestResolve_Name(t *testing.T) {
	directory := fakeUsers(
		// Display name is a handle; only the Slack real name carries the full name.
		userNamed(t, "pat@example.com", "pat", "pat", "Pat Quinlan"),
		userNamed(t, "jose@example.com", "squid", "squid", "José Núñez Peña"),
		userNamed(t, "kim.lee@example.com", "Kim Lee", "", ""),
		// Two different people recorded under one full name.
		userNamed(t, "sam.ortiz@example.com", "Sam Ortiz", "", ""),
		userNamed(t, "sortiz@example.com", "", "sortiz", "Sam Ortiz"),
	)

	cases := []struct {
		name        string
		ref         string
		wantSubject string
	}{
		{name: "full name on the Slack real name resolves", ref: "name:Pat Quinlan", wantSubject: mustEmailSubject(t, "pat@example.com")},
		{name: "case and spacing are ignored", ref: "name:  pat   QUINLAN ", wantSubject: mustEmailSubject(t, "pat@example.com")},
		{name: "accents are ignored", ref: "name:Jose Nunez Pena", wantSubject: mustEmailSubject(t, "jose@example.com")},
		{name: "full name on spec.displayName resolves", ref: "name:Kim Lee", wantSubject: mustEmailSubject(t, "kim.lee@example.com")},
		{name: "two users share the name: unresolved", ref: "name:Sam Ortiz"},
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
	ui := userNamed(t, "kim.lee@example.com", "Kim Lee", "kim", "Kim Lee")
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
