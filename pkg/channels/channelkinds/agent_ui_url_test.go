package channelkinds

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestComposeAgentUIURL covers the shape ComposeAgentUIURL guarantees: the
// session rides the query string (never a path segment), a trailing slash on
// the base doesn't double, an unpopulated base is a clean skip, and a
// malformed sessionRef is a caller bug reported as an error.
func TestComposeAgentUIURL(t *testing.T) {
	cases := []struct {
		name       string
		base       string
		sessionRef string
		wantURL    string
		wantErr    bool
		errSubstr  string
	}{
		{
			name:       "happy path carries the session as a query parameter",
			base:       "https://webd.example.test",
			sessionRef: "demo-ns/demo-session",
			wantURL:    "https://webd.example.test/sessions?session=demo-ns%2Fdemo-session",
		},
		{
			name:       "a trailing slash on the base does not double",
			base:       "https://webd.example.test/",
			sessionRef: "demo-ns/demo-session",
			wantURL:    "https://webd.example.test/sessions?session=demo-ns%2Fdemo-session",
		},
		{
			name:       "an unpopulated base is a clean skip, not an error",
			base:       "",
			sessionRef: "demo-ns/demo-session",
			wantURL:    "",
		},
		{
			name:       "a sessionRef with no slash is a caller bug",
			base:       "https://webd.example.test",
			sessionRef: "demo-session",
			wantErr:    true,
			errSubstr:  "sessionRef",
		},
		{
			name:       "an empty namespace is a caller bug",
			base:       "https://webd.example.test",
			sessionRef: "/demo-session",
			wantErr:    true,
			errSubstr:  "sessionRef",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComposeAgentUIURL(tc.base, tc.sessionRef)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL, got)
		})
	}
}

// TestComposeAgentUIURLUsesTheQueryForm is the guard for the query-form
// routing decision: the shell's APIs live under /sessions/api/…, so a
// path-form session address is shadowed for a namespace named "api" — the
// address would resolve to an API route instead of the page. Asserting the
// prefix is what distinguishes the two forms — the happy-path row above
// would also pass for "/sessions/demo-ns/demo-session" if it were written
// as a Contains.
func TestComposeAgentUIURLUsesTheQueryForm(t *testing.T) {
	got, err := ComposeAgentUIURL("https://webd.example.test", "api/demo-session")
	require.NoError(t, err, "a namespace named \"api\" is legal and must compose")
	assert.True(t, strings.HasPrefix(got, "https://webd.example.test/sessions?"),
		"the session must be a query parameter, never a path segment")
	assert.NotContains(t, got, "/sessions/api",
		"a path form would collide with the shell's own /sessions/api/… routes")
}
