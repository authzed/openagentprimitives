package channelkinds

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComposeSessionViewURL_HappyPath verifies the exact URL shape every
// SessionViewMinter implementation relies on.
func TestComposeSessionViewURL_HappyPath(t *testing.T) {
	u, err := ComposeSessionViewURL("https://webd.example.com", "demo-ns/demo-sess")
	require.NoError(t, err)
	assert.Equal(t, "https://webd.example.com/session-view/demo-ns/demo-sess", u)
}

// TestComposeSessionViewURL_TrimsTrailingSlash verifies a trailing slash on
// the base doesn't produce a doubled "//" in the composed URL.
func TestComposeSessionViewURL_TrimsTrailingSlash(t *testing.T) {
	u, err := ComposeSessionViewURL("https://webd.example.com/", "demo-ns/demo-sess")
	require.NoError(t, err)
	assert.Equal(t, "https://webd.example.com/session-view/demo-ns/demo-sess", u)
}

// TestComposeSessionViewURL_EmptyBaseCleanSkip verifies an empty base (webd
// not yet configured) yields ("", nil), not an error — callers treat this as
// a clean skip.
func TestComposeSessionViewURL_EmptyBaseCleanSkip(t *testing.T) {
	u, err := ComposeSessionViewURL("", "demo-ns/demo-sess")
	require.NoError(t, err)
	assert.Empty(t, u)
}

// TestComposeSessionViewURL_MalformedSessionRef verifies a sessionRef missing
// the "ns/name" shape is a hard error, not a silently broken URL — this
// indicates a caller bug, not a runtime configuration gap.
func TestComposeSessionViewURL_MalformedSessionRef(t *testing.T) {
	cases := []struct {
		name       string
		sessionRef string
	}{
		{name: "no slash", sessionRef: "demo-sess"},
		{name: "empty ns", sessionRef: "/demo-sess"},
		{name: "empty name", sessionRef: "demo-ns/"},
		{name: "empty string", sessionRef: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ComposeSessionViewURL("https://webd.example.com", tc.sessionRef)
			assert.Error(t, err)
		})
	}
}

// TestComposeSessionViewURL_PathEscapesSegments verifies ns/name segments are
// escaped, so a segment containing a reserved path character cannot smuggle
// extra path segments into the composed URL.
func TestComposeSessionViewURL_PathEscapesSegments(t *testing.T) {
	u, err := ComposeSessionViewURL("https://webd.example.com", "demo ns/demo sess")
	require.NoError(t, err)
	assert.Equal(t, "https://webd.example.com/session-view/demo%20ns/demo%20sess", u)
}
