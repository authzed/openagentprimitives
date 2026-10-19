// pkg/platform/identity/setup/refresh_internal_test.go
//
// Unit tests for the refreshFilter's internals (consider / unmatchedErr) that
// are awkward to drive end-to-end through the engine: the two-providers case
// needs two requirements sharing a credential name, and the error-message
// shapes are easier to pin directly than to construct a fixture for each.
package setup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An inert filter (no --refresh) runs every credential and records nothing.
func TestRefreshFilterInertWhenNoWant(t *testing.T) {
	f := &refreshFilter{}
	assert.False(t, f.active())
	assert.True(t, f.consider("anything"), "inert filter runs every credential")
	assert.True(t, f.consider("another"))
	assert.Empty(t, f.candidates, "inert filter records no candidates")
	assert.NoError(t, f.unmatchedErr())
}

// The named credential is run exactly once even when it is required through two
// providers (which reach consider twice, since the engine dedups on
// name|provider). Pins the comment-2 fix.
func TestRefreshFilterRunsNamedCredentialOnce(t *testing.T) {
	f := &refreshFilter{want: "tailscale-authkey"}
	assert.False(t, f.consider("github-token"), "non-matching credential is skipped")
	assert.True(t, f.consider("tailscale-authkey"), "first match runs")
	assert.False(t, f.consider("tailscale-authkey"), "second match (other provider) must NOT run again")
	require.NoError(t, f.unmatchedErr(), "a matched refresh is not an error")
}

// A near-miss name fails with the closest candidate and lists what is available.
func TestRefreshFilterUnmatchedSuggests(t *testing.T) {
	f := &refreshFilter{want: "tailscale-authky"} // one edit from tailscale-authkey
	f.consider("github-token")
	f.consider("tailscale-authkey")
	err := f.unmatchedErr()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did you mean")
	assert.Contains(t, err.Error(), "tailscale-authkey")
	assert.Contains(t, err.Error(), "available: github-token, tailscale-authkey")
}

// A correct name that --only filtered out still lands as unmatched, so the
// error names the --only scope rather than blaming the name. Pins comment 3.
func TestRefreshFilterUnmatchedNamesOnlyScope(t *testing.T) {
	f := &refreshFilter{want: "tailscale-authkey", only: "mcp:linear"}
	f.consider("linear-oauth") // the only credential --only left visible
	err := f.unmatchedErr()
	require.Error(t, err)
	assert.Contains(t, err.Error(), `within --only "mcp:linear"`)
	assert.Contains(t, err.Error(), "linear-oauth")
}

// With no candidates at all (the --only target carried no credentials) the
// error still reports the --only scope.
func TestRefreshFilterUnmatchedNoCandidates(t *testing.T) {
	f := &refreshFilter{want: "x", only: "mcp:linear"}
	err := f.unmatchedErr()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no credentials to refresh")
	assert.Contains(t, err.Error(), `within --only "mcp:linear"`)
}
