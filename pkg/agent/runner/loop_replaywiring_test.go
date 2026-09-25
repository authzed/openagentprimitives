package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// workCompleteScript ends the run on the first turn, so exactly one request
// reaches the provider and the assertion is about that request.
func workCompleteScript() []llmfake.Step {
	return []llmfake.Step{{Resp: llm.Response{
		Content: []llm.ContentBlock{{Type: "tool_use", ToolUse: &llm.ToolUseBlock{
			ID: "tu_1", Name: "agent_work_complete", Input: json.RawMessage(`{"summary":"done"}`),
		}}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}}
}

// TestLoop_ReplayToolCatalogNarrowsTheREQUEST is the wiring assertion, and it
// is separate from the unit test on replayToolDefs on purpose: a replay's whole
// point is that the model is offered the set the captured run was offered, and
// building the narrowed list and then sending the wide one would be invisible
// to a scripted transcript — the stub answers from the script no matter what it
// was offered.
func TestLoop_ReplayToolCatalogNarrowsTheREQUEST(t *testing.T) {
	l, provider, _, _ := newLoopFixture(t, workCompleteScript())

	var sawTurnIndex int
	var sawOffered []string
	l.ReplayToolCatalog = func(turnIndex int, offered []string) []string {
		sawTurnIndex, sawOffered = turnIndex, offered
		return []string{"agent_work_complete"}
	}

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")), "Run must succeed")

	reqs := provider.Requests()
	require.Len(t, reqs, 1, "expected exactly one LLM call")
	names := make([]string, 0, len(reqs[0].Tools))
	for _, d := range reqs[0].Tools {
		names = append(names, d.Name)
	}
	assert.Equal(t, []string{"agent_work_complete"}, names,
		"the request must carry the narrowed set, not the composed one")

	assert.Greater(t, len(sawOffered), 1, "the seam is shown every tool the run composed")
	assert.Contains(t, sawOffered, "agent_work_complete")
	assert.Equal(t, 1, sawTurnIndex,
		"the seam is asked about the transcript index this request's assistant turn lands at")
}

// Production, and every scenario that pins no catalog: a nil seam leaves the
// request carrying everything the run composed.
func TestLoop_NilReplayToolCatalogOffersEverything(t *testing.T) {
	l, provider, _, _ := newLoopFixture(t, workCompleteScript())
	require.Nil(t, l.ReplayToolCatalog, "nil is what every production binary passes")

	require.NoError(t, l.Run(memory.WithSystemApproval(context.Background(), "test")), "Run must succeed")

	reqs := provider.Requests()
	require.Len(t, reqs, 1)
	assert.Greater(t, len(reqs[0].Tools), 1, "an unpinned run offers its whole composed tool set")
}
