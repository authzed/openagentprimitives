package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerOwnerSubject_NamesThePRAuthorByAccountID pins the subject-set a
// verified pull_request delivery yields for the session's additional owner:
// the author's NUMERIC account id, never the login — logins are mutable and a
// released login can be claimed by another account, which for an owner tuple
// is a privilege transfer waiting to happen. The id keys the same github_user
// type the attested identity edge binds, so the tuple resolves to a platform
// user exactly when that person has linked a verified GitHub credential.
//
// Driven from the package's real-payload fixtures, same as TriggerFacts: a
// synthetic body marshaled from prEvent would pass even with the json tags
// wrong.
func TestTriggerOwnerSubject_NamesThePRAuthorByAccountID(t *testing.T) {
	subject, ok, err := receiver{}.TriggerOwnerSubject(testChannel(t), "pull_request", fixtureBody(t, "pull_request_opened.json"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "github_user:4172237#user", subject)
}

// A fork pull request's author is named the same way — deliberately uniform.
// The session exists either way (forks deliver and the agent declines them
// visibly), the subject pays off only through a verified attested edge for the
// author's OWN account, and the author seeing why their PR was declined is the
// point of the visible refusal.
func TestTriggerOwnerSubject_UniformForForkPRs(t *testing.T) {
	subject, ok, err := receiver{}.TriggerOwnerSubject(testChannel(t), "pull_request", fixtureBody(t, "pull_request_fork.json"))
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "github_user:9906581#user", subject)
}

func TestTriggerOwnerSubject_YieldsNothingRatherThanGarbage(t *testing.T) {
	cases := []struct {
		name  string
		event string
		body  string
	}{
		// A zero or absent id must never become "github_user:0#user" — an id
		// no real account holds today is exactly the kind of subject a future
		// account could be minted onto.
		{name: "payload without an author id", event: "pull_request",
			body: `{"action":"opened","number":7,"pull_request":{"user":{"login":"demo-user"}},"repository":{"full_name":"demo-org/platform"}}`},
		{name: "a non-pull_request event", event: "ping",
			body: `{"zen":"Keep it logically awesome."}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, ok, err := receiver{}.TriggerOwnerSubject(testChannel(t), tc.event, []byte(tc.body))
			require.NoError(t, err)
			assert.False(t, ok)
			assert.Empty(t, subject)
		})
	}
}

func TestTriggerOwnerSubject_UndecodableBodyErrors(t *testing.T) {
	_, ok, err := receiver{}.TriggerOwnerSubject(testChannel(t), "pull_request", []byte("not json"))
	require.Error(t, err)
	assert.False(t, ok)
}
