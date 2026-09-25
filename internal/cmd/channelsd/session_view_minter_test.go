// Tests for the channelsd session-view minter: unlike the artifact-view
// minter this composes a plain, unsigned URL — no passthroughlink.Signer
// involved — so the tests exercise URL composition directly rather than
// signature round-tripping.
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
)

// TestNewSessionViewMinter_SatisfiesInterface verifies that
// newSessionViewMinter returns a value that satisfies
// channelkinds.SessionViewMinter.
func TestNewSessionViewMinter_SatisfiesInterface(t *testing.T) {
	m := newSessionViewMinter(func() string { return "https://webd.example" })
	var _ channelkinds.SessionViewMinter = m
}

// TestNewSessionViewMinter_HappyPath verifies the exact URL shape from the
// task spec: MintSessionViewLink("ns1/sess1", subj, "") ->
// "https://webd.example/session-view/ns1/sess1" — a plain path, no ?d=&sig=.
func TestNewSessionViewMinter_HappyPath(t *testing.T) {
	m := newSessionViewMinter(func() string { return "https://webd.example" })

	subj := identity.RawSubject("user:alice")
	u, err := m.MintSessionViewLink("ns1/sess1", subj, "")
	require.NoError(t, err, "MintSessionViewLink should succeed")
	assert.Equal(t, "https://webd.example/session-view/ns1/sess1", u,
		"URL must be a plain path with no ?d=&sig= — access is CheckInteract at open time, not a signed capability")
}

// TestNewSessionViewMinter_TrimsTrailingSlash verifies a trailing slash on
// the base URL doesn't produce a double slash.
func TestNewSessionViewMinter_TrimsTrailingSlash(t *testing.T) {
	m := newSessionViewMinter(func() string { return "https://webd.example/" })
	u, err := m.MintSessionViewLink("ns1/sess1", identity.Principal{}, "")
	require.NoError(t, err)
	assert.Equal(t, "https://webd.example/session-view/ns1/sess1", u)
}

// TestNewSessionViewMinter_EscapesSegments verifies namespace/name segments
// are URL-escaped independently.
func TestNewSessionViewMinter_EscapesSegments(t *testing.T) {
	m := newSessionViewMinter(func() string { return "https://webd.example" })
	u, err := m.MintSessionViewLink("my ns/my sess", identity.Principal{}, "")
	require.NoError(t, err)
	assert.Equal(t, "https://webd.example/session-view/my%20ns/my%20sess", u)
}

// TestNewSessionViewMinter_EmptyBaseURL verifies that a missing webd base
// URL returns ("", nil) — a clean skip, not an error.
func TestNewSessionViewMinter_EmptyBaseURL(t *testing.T) {
	m := newSessionViewMinter(func() string { return "" })
	u, err := m.MintSessionViewLink("ns1/sess1", identity.Principal{}, "")
	require.NoError(t, err, "empty base URL must return nil error (clean skip)")
	assert.Empty(t, u, "empty base URL must return empty URL string")
}

// TestNewSessionViewMinter_MalformedSessionRef_ReturnsError verifies a
// sessionRef without a "/" is rejected rather than producing a malformed URL.
func TestNewSessionViewMinter_MalformedSessionRef_ReturnsError(t *testing.T) {
	m := newSessionViewMinter(func() string { return "https://webd.example" })
	_, err := m.MintSessionViewLink("no-slash", identity.Principal{}, "")
	require.Error(t, err, "sessionRef without ns/name must be rejected")
	assert.Contains(t, err.Error(), "sessionRef")
}
