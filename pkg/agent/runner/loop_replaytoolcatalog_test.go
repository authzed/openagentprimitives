package runner

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

func defNames(defs []llm.ToolDef) []string {
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, d.Name)
	}
	return out
}

func replayLoop(seam func(int, []string) []string) *Loop {
	return &Loop{
		Tools: []tool.Tool{
			&fakeMCPTool{name: "respond_to_user"},
			&fakeMCPTool{name: "box_read"},
			&fakeMCPTool{name: "box_write"},
		},
		ReplayToolCatalog: seam,
	}
}

func TestReplayToolDefs_OffersOnlyWhatTheSeamKeeps(t *testing.T) {
	l := replayLoop(func(turnIndex int, offered []string) []string {
		assert.Equal(t, 7, turnIndex, "the seam is asked about the turn the request lands at")
		assert.Equal(t, []string{"respond_to_user", "box_read", "box_write"}, offered,
			"the seam sees every name on the request, in request order")
		return []string{"respond_to_user"}
	})

	got := l.replayToolDefs(7, l.buildToolDefs())

	assert.Equal(t, []string{"respond_to_user"}, defNames(got))
}

// The cache breakpoint follows the SUBSET's own last client-side tool. Left on
// a def that was filtered out, the request carries no breakpoint at all and
// every replayed turn re-processes the whole tool list at full price.
func TestReplayToolDefs_MovesTheCacheBreakpointToTheNewLastTool(t *testing.T) {
	l := replayLoop(func(int, []string) []string {
		return []string{"respond_to_user", "box_read"}
	})

	got := l.replayToolDefs(1, l.buildToolDefs())

	require.Len(t, got, 2)
	assert.False(t, got[0].Cacheable)
	assert.True(t, got[1].Cacheable, "the breakpoint must move to the subset's last tool")
}

// Provider server-tool defs are dispatched by the LLM API, not by the runner,
// so they are not in l.Tools and cannot be narrowed. They still reach the seam,
// because that is the list the tool_catalog Kind records — the seam reports
// them and they ride through, which is honest rather than silent.
func TestReplayToolDefs_ServerToolDefsAreSeenButRideThrough(t *testing.T) {
	var sawOffered []string
	l := replayLoop(func(_ int, offered []string) []string {
		sawOffered = offered
		return []string{"respond_to_user"}
	})
	l.ExtraToolDefs = []llm.ToolDef{{Name: "web_search_20250305"}}

	got := l.replayToolDefs(1, l.buildToolDefs())

	assert.Contains(t, sawOffered, "web_search_20250305", "the seam must be told about a server tool")
	assert.Equal(t, []string{"respond_to_user", "web_search_20250305"}, defNames(got))
}

// Production, and every scenario that pins no catalog: buildToolDefs is what it
// always was, and buildToolDefsFrom over l.Tools must be identical to it.
func TestBuildToolDefs_IsBuildToolDefsFromOverTheLiveSet(t *testing.T) {
	l := replayLoop(nil)
	l.ExtraToolDefs = []llm.ToolDef{{Name: "web_search_20250305"}}

	assert.Equal(t, l.buildToolDefs(), buildToolDefsFrom(l.Tools, l.ExtraToolDefs))
	assert.Equal(t, []string{"respond_to_user", "box_read", "box_write", "web_search_20250305"},
		defNames(l.buildToolDefs()))
	require.Nil(t, l.ReplayToolCatalog, "a nil seam is what every production binary passes")
}
