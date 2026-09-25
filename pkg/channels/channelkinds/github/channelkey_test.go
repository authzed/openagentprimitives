package github

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChannelKey_RoundTrips is the reason the writer and the reader share a
// file: the binding key is how the trigger-status surface learns which
// repository and which pull request it is answering for, so a format only one
// side knows about would make every existing session's check run unaddressable
// with nothing failing at build time.
func TestChannelKey_RoundTrips(t *testing.T) {
	cases := []struct {
		name     string
		fullName string
		number   int
	}{
		{name: "ordinary repository", fullName: "demo-org/platform", number: 42},
		{name: "a repository name containing a dash", fullName: "demo-org/some-repo", number: 1},
		{name: "a large pull request number", fullName: "demo-org/platform", number: 987654},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := parseChannelKey(formatChannelKey(tc.fullName, tc.number))
			require.NoError(t, err)
			assert.Equal(t, tc.fullName, owner+"/"+repo)
			assert.Equal(t, tc.number, number)
		})
	}
}

// TestParseChannelKey_RefusesWhatIsNotAPullRequest. Each of these would, if
// guessed at, address a repository nobody named.
func TestParseChannelKey_RefusesWhatIsNotAPullRequest(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "another kind's thread key", key: "thread:C123:1699999999.000100"},
		{name: "no number", key: "pr:demo-org/platform"},
		{name: "no owner", key: "pr:platform#1"},
		{name: "a number that is not a number", key: "pr:demo-org/platform#latest"},
		{name: "a zero number, which Translate already refuses to emit", key: "pr:demo-org/platform#0"},
		{name: "empty", key: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := parseChannelKey(tc.key)
			require.Error(t, err)
		})
	}
}
