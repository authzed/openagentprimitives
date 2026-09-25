// Tests for the channelsd agent-UI minter. Like the session-view minter and
// unlike the artifact-view minter, this composes a plain unsigned URL — no
// passthroughlink.Signer is involved — so these exercise URL composition and
// the not-configured branches directly rather than signature round-tripping.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// The constructor's result must satisfy channelkinds.AgentUIMinter, which is
// what channelsd's run() hands to SetAgentUIMinter. This is a COMPILE-time
// pin — a runtime assertion here could only restate that a constructor which
// unconditionally returns &agentUIMinter{} returned something — so a
// signature drift on either side fails the build rather than a test.
var _ channelkinds.AgentUIMinter = (*agentUIMinter)(nil)

// TestAgentUIMinterComposesTheShellURL pins the URL the button carries: the
// shell page with the session as a QUERY parameter. The path form would be
// shadowed by the shell's own "/sessions/api/…" routes for a namespace named
// "api" — see channelkinds.ComposeAgentUIURL.
func TestAgentUIMinterComposesTheShellURL(t *testing.T) {
	m := newAgentUIMinter(func() string { return "https://webd.example" })

	got, err := m.MintAgentUILink("demo-ns/demo-session")
	require.NoError(t, err, "a well-formed sessionRef and base URL must compose")
	assert.Equal(t, "https://webd.example/sessions?session=demo-ns%2Fdemo-session", got,
		"the offer must link to the shell with the session in the query, not as a path segment")
}

// TestAgentUIMinterEmptyBaseURLIsACleanSkip covers the state channelsd boots
// in: the minter is wired unconditionally, before webd's trusted-URL
// ConfigMap is necessarily populated. ("", nil) lets the sender report
// "not configured yet" at the moment an offer needs it, rather than failing
// the whole outbound envelope.
func TestAgentUIMinterEmptyBaseURLIsACleanSkip(t *testing.T) {
	m := newAgentUIMinter(func() string { return "" })

	got, err := m.MintAgentUILink("demo-ns/demo-session")
	require.NoError(t, err, "an unpopulated webd URL is a normal startup state, not an error")
	assert.Empty(t, got, "an unpopulated webd URL must compose to no URL at all")
}

// TestAgentUIMinterReportsAMissingGetterAsAnError separates the two
// not-configured states. An empty base URL is normal and silent; a minter
// whose getter was never wired is a bug in run(), and must not be reported
// the same way.
func TestAgentUIMinterReportsAMissingGetterAsAnError(t *testing.T) {
	var unconstructed *agentUIMinter

	_, nilErr := unconstructed.MintAgentUILink("demo-ns/demo-session")
	assert.Error(t, nilErr, "a nil minter must answer with a cause, not an empty URL")

	_, noGetterErr := (&agentUIMinter{}).MintAgentUILink("demo-ns/demo-session")
	assert.Error(t, noGetterErr, "a minter with no base-URL getter must answer with a cause")
}

// TestAgentUIMinterRejectsAMalformedSessionRef: a sessionRef with no "ns/name"
// split is a caller bug, and must surface as an error rather than a URL that
// opens the shell on nothing.
func TestAgentUIMinterRejectsAMalformedSessionRef(t *testing.T) {
	m := newAgentUIMinter(func() string { return "https://webd.example" })

	_, err := m.MintAgentUILink("no-slash")
	require.Error(t, err, "a sessionRef without ns/name must be rejected")
	assert.Contains(t, err.Error(), "sessionRef",
		"the error must name what was malformed, so an operator can act on it")
}
