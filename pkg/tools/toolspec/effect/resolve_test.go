package effect

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/parser"
	"github.com/authzed/openagentprimitives/pkg/tools/toolspec/toolkit"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name       string
		effects    toolkit.Effects
		call       *parser.Call
		env        map[string]string
		cwd        string
		assertions func(t *testing.T, r Resolved)
	}{
		{
			name: "static fields passthrough unchanged",
			effects: toolkit.Effects{
				Destructive: false,
				Reads:       []string{"network"},
				Writes:      []string{},
				Network:     toolkit.NetworkEffect{Destinations: []string{"api.github.com"}},
				Filesystem:  toolkit.FilesystemEffect{Paths: []string{}},
				Creds:       toolkit.CredsEffect{Required: []string{"GITHUB_TOKEN"}, Writes: []string{}},
			},
			call: &parser.Call{Flags: map[string]any{}, Positional: map[string]any{}},
			env:  map[string]string{},
			cwd:  "/work",
			assertions: func(t *testing.T, r Resolved) {
				assert.Equal(t, []string{"api.github.com"}, r.Network.Destinations)
				assert.Equal(t, []string{"GITHUB_TOKEN"}, r.Creds.Required)
			},
		},
		{
			name: "{flag:hostname} resolves from Call.Flags",
			effects: toolkit.Effects{
				Network: toolkit.NetworkEffect{Destinations: []string{"api.github.com", "{flag:hostname}"}},
			},
			call: &parser.Call{Flags: map[string]any{"hostname": "enterprise.com"}},
			assertions: func(t *testing.T, r Resolved) {
				assert.Equal(t, []string{"api.github.com", "enterprise.com"}, r.Network.Destinations)
			},
		},
		{
			name: "{flag:hostname} with no value is dropped",
			effects: toolkit.Effects{
				Network: toolkit.NetworkEffect{Destinations: []string{"api.github.com", "{flag:hostname}"}},
			},
			call: &parser.Call{Flags: map[string]any{}},
			assertions: func(t *testing.T, r Resolved) {
				assert.Equal(t, []string{"api.github.com"}, r.Network.Destinations)
			},
		},
		{
			name: "{cwd}, {env:HOME}, {positional:target} all resolve",
			effects: toolkit.Effects{
				Filesystem: toolkit.FilesystemEffect{Paths: []string{"{cwd}", "{env:HOME}/out", "{positional:target}"}},
			},
			call: &parser.Call{Positional: map[string]any{"target": "/tmp/out"}},
			env:  map[string]string{"HOME": "/root"},
			cwd:  "/work",
			assertions: func(t *testing.T, r Resolved) {
				assert.Equal(t, []string{"/work", "/root/out", "/tmp/out"}, r.Filesystem.Paths)
			},
		},
		{
			name: "malformed colon-less destination template is force-denied, not dropped",
			effects: toolkit.Effects{
				Network: toolkit.NetworkEffect{Destinations: []string{"api.github.com", "{hostname}"}},
			},
			call: &parser.Call{Flags: map[string]any{}},
			assertions: func(t *testing.T, r Resolved) {
				// The malformed entry must survive resolution as a value that
				// no allow-list can contain, so the egress gate denies it.
				require.Len(t, r.Network.Destinations, 2)
				assert.Equal(t, "api.github.com", r.Network.Destinations[0])
				assert.True(t, IsUnresolved(r.Network.Destinations[1]),
					"malformed template must resolve to an un-allowlistable sentinel, got %q", r.Network.Destinations[1])
			},
		},
		{
			name: "unknown-kind destination template is force-denied, not dropped",
			effects: toolkit.Effects{
				Network: toolkit.NetworkEffect{Destinations: []string{"{secret:apihost}"}},
			},
			call: &parser.Call{Flags: map[string]any{}},
			assertions: func(t *testing.T, r Resolved) {
				require.Len(t, r.Network.Destinations, 1)
				assert.True(t, IsUnresolved(r.Network.Destinations[0]),
					"unknown kind must resolve to an un-allowlistable sentinel, got %q", r.Network.Destinations[0])
			},
		},
		{
			name: "unknown-kind filesystem path is force-denied, not dropped",
			effects: toolkit.Effects{
				Filesystem: toolkit.FilesystemEffect{Paths: []string{"{secret:dir}/out"}},
			},
			call: &parser.Call{},
			assertions: func(t *testing.T, r Resolved) {
				require.Len(t, r.Filesystem.Paths, 1)
				assert.True(t, IsUnresolved(r.Filesystem.Paths[0]),
					"unknown kind must resolve to an un-allowlistable sentinel, got %q", r.Filesystem.Paths[0])
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Resolve(tc.effects, tc.call, tc.env, tc.cwd)
			tc.assertions(t, r)
		})
	}
}

// TestResolve_FailClosedAgainstAllowList proves the security property the audit
// flagged: a misauthored/unresolvable effect template must NOT vacuously satisfy
// an allow-list. We mirror the validator's allow-list membership logic
// (deny-unless-listed) and assert the unresolved entry is flagged as "not allowed".
func TestResolve_FailClosedAgainstAllowList(t *testing.T) {
	// notSubset mirrors validator/rules.go notSubset: returns actual entries
	// absent from the allow list.
	notSubset := func(actual, allowed []string) []string {
		m := map[string]bool{}
		for _, a := range allowed {
			m[a] = true
		}
		var out []string
		for _, a := range actual {
			if !m[a] {
				out = append(out, a)
			}
		}
		return out
	}

	cases := []struct {
		name    string
		effects toolkit.Effects
		call    *parser.Call
		env     map[string]string
	}{
		{
			name:    "colon-less template",
			effects: toolkit.Effects{Network: toolkit.NetworkEffect{Destinations: []string{"{hostname}"}}},
			call:    &parser.Call{Flags: map[string]any{}},
		},
		{
			name:    "unknown kind",
			effects: toolkit.Effects{Network: toolkit.NetworkEffect{Destinations: []string{"{secret:apihost}"}}},
			call:    &parser.Call{Flags: map[string]any{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := Resolve(tc.effects, tc.call, tc.env, "")
			// An allow-list that, pre-fix, the empty (dropped) result vacuously
			// satisfied. Post-fix the sentinel must be flagged as not-allowed.
			allowed := []string{"api.github.com"}
			extra := notSubset(r.Network.Destinations, allowed)
			require.NotEmpty(t, extra,
				"unresolvable destination must be flagged as not in the allow list (fail closed), got resolved=%v", r.Network.Destinations)
		})
	}
}

// TestUnresolvedSentinelNeverMatchesAllowList guards the sentinel itself: no
// realistic allow-list entry or path prefix can equal/contain it.
func TestUnresolvedSentinelNeverMatchesAllowList(t *testing.T) {
	s, ok := expander{}.resolveTmpl("hostname") // colon-less -> sentinel
	require.False(t, ok, "malformed template must not report resolved")
	require.True(t, IsUnresolved(s))
	assert.NotEqual(t, "api.github.com", s)
	assert.NotEqual(t, "/", s)
}
