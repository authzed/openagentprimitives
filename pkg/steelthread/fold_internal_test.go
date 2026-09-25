package steelthread

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSplitMCPName pins the mapping between the name the model calls and the
// name the replay registers a handler under.
//
// Worth its own test rather than being left to the end-to-end folds: getting it
// backwards fails at replay as "unknown tool", which mentions neither the
// prefix nor the mapping, so the symptom points nowhere near the cause.
func TestSplitMCPName(t *testing.T) {
	cases := []struct {
		name       string
		llmFacing  string
		prefixes   []string
		wantIsMCP  bool
		wantServer string
	}{
		{
			name:      "a declared prefix is stripped to the server-side name",
			llmFacing: "acme_list_widgets", prefixes: []string{"acme"},
			wantIsMCP: true, wantServer: "list_widgets",
		},
		{
			name:      "the server-side name keeps its own underscores",
			llmFacing: "acme_list_all_widgets", prefixes: []string{"acme"},
			wantIsMCP: true, wantServer: "list_all_widgets",
		},
		{
			name:      "the second declared server is matched as readily as the first",
			llmFacing: "other_list_widgets", prefixes: []string{"acme", "other"},
			wantIsMCP: true, wantServer: "list_widgets",
		},
		{
			name:      "an undeclared prefix is not MCP and keeps its whole name",
			llmFacing: "other_list_widgets", prefixes: []string{"acme"},
			wantIsMCP: false, wantServer: "other_list_widgets",
		},
		{
			// The reason isMCP is read from the class rather than guessed: a
			// meta tool's name contains an underscore too, and guessing would
			// be wrong in the direction that hands a canned output to a tool
			// that runs for real.
			name:      "a meta tool whose name merely contains an underscore is not MCP",
			llmFacing: "update_plan", prefixes: []string{"acme"},
			wantIsMCP: false, wantServer: "update_plan",
		},
		{
			name:      "the prefix must be followed by an underscore, not merely begin the name",
			llmFacing: "acmelist_widgets", prefixes: []string{"acme"},
			wantIsMCP: false, wantServer: "acmelist_widgets",
		},
		{
			name:      "a class declaring no MCP servers resolves nothing as MCP",
			llmFacing: "acme_list_widgets", prefixes: nil,
			wantIsMCP: false, wantServer: "acme_list_widgets",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isMCP, server := splitMCPName(tc.llmFacing, tc.prefixes)
			assert.Equal(t, tc.wantIsMCP, isMCP)
			assert.Equal(t, tc.wantServer, server)
		})
	}
}
