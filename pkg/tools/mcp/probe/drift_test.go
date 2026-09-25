package probe_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/tools/mcp/probe"
)

// served builds a tools/list result from bare names.
func served(names ...string) []probe.Tool {
	out := make([]probe.Tool, 0, len(names))
	for _, n := range names {
		out = append(out, probe.Tool{Name: n})
	}
	return out
}

func TestAllowlistDrift(t *testing.T) {
	cases := []struct {
		name        string
		pinned      []string
		live        []probe.Tool
		wantMissing []string
		// wantIn are substrings the rendered message must carry; empty
		// wantMissing means the message must be empty too.
		wantIn    []string
		wantNotIn []string
	}{
		{
			name:   "every pinned tool served: no drift, no message",
			pinned: []string{"b", "a"},
			live:   served("a", "b", "c"),
		},
		{
			name:        "one pinned tool absent: names it AND what the server does serve",
			pinned:      []string{"get_pull_request"},
			live:        served("pull_request_read", "list_pull_requests"),
			wantMissing: []string{"get_pull_request"},
			wantIn: []string{
				"demo-connector",
				"get_pull_request",
				"pull_request_read",
				"list_pull_requests",
			},
		},
		{
			name:        "several absent: missing names are sorted, not in spec order",
			pinned:      []string{"zeta", "alpha", "alpha"},
			live:        served("other"),
			wantMissing: []string{"alpha", "zeta"},
			wantIn:      []string{"[alpha, zeta]", "other"},
		},
		{
			name:        "server serves nothing at all: says so rather than printing an empty list",
			pinned:      []string{"only_one"},
			live:        nil,
			wantMissing: []string{"only_one"},
			wantIn:      []string{"only_one", "offers no tools at all"},
			wantNotIn:   []string{"offers: []"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			missing, msg := probe.AllowlistDrift("demo-connector", tc.pinned, tc.live)
			assert.Equal(t, tc.wantMissing, missing)
			if len(tc.wantMissing) == 0 {
				assert.Empty(t, msg, "no drift must render no message")
				return
			}
			for _, want := range tc.wantIn {
				assert.Contains(t, msg, want)
			}
			for _, notWant := range tc.wantNotIn {
				assert.NotContains(t, msg, notWant)
			}
		})
	}
}

// TestAllowlistDriftCapsTheServedList keeps a server with hundreds of tools
// from turning a status condition and an append-only log entry into a dump:
// the point of naming what IS served is to let a reader spot the renamed tool,
// and forty sorted names does that.
func TestAllowlistDriftCapsTheServedList(t *testing.T) {
	var names []string
	for _, c := range "abcdefghijklmnopqrstuvwxyz" {
		for i := 0; i < 3; i++ {
			names = append(names, string(c)+string(rune('0'+i))+"_tool")
		}
	}
	require.Len(t, names, 78)

	missing, msg := probe.AllowlistDrift("demo-connector", []string{"absent"}, served(names...))
	require.Equal(t, []string{"absent"}, missing)

	assert.Contains(t, msg, "a0_tool", "the first sorted served name is kept")
	assert.NotContains(t, msg, "z2_tool", "names past the cap are dropped, not rendered")
	assert.Contains(t, msg, "78", "the message says how many the server actually serves")
	assert.Equal(t, 40, strings.Count(msg, "_tool"), "exactly the cap's worth of served names")
}

// TestAllowlistDriftCapsPathologicalNameLength proves the whole-message cap
// holds even when the count cap (40) is not enough on its own: a served name
// is the connector's own untrusted upstream input, and this message lands on
// both an AgentSession Failed condition (CRD maxLength 32768 — a status patch
// past that crash-loops the runner into RunnerCrash) and an append-only
// signed log entry that can never be deleted once written.
func TestAllowlistDriftCapsPathologicalNameLength(t *testing.T) {
	names := make([]string, 40)
	for i := range names {
		// Unique 2-digit prefix so dedup-by-name never collapses these; padded
		// to 1000 runes each with a repeated filler.
		names[i] = fmt.Sprintf("%02d", i) + strings.Repeat("z", 998)
	}

	missing, msg := probe.AllowlistDrift("demo-connector", []string{"absent"}, served(names...))
	require.Equal(t, []string{"absent"}, missing)

	assert.LessOrEqual(t, len([]rune(msg)), 2000, "the rendered message must never exceed the cap")
	assert.True(t, strings.HasPrefix(msg, "MCPServer/demo-connector: pinned tools not served: [absent];"),
		"the missing pin is named first and must survive the cap intact")
}
