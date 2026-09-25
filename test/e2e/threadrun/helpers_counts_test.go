//go:build e2e

package threadrun

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	bt "github.com/authzed/openagentprimitives/pkg/bronzethread"
)

func TestToolResultCountsMatchRequiresTheExactLatestPopulation(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{
		{Role: "assistant", Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "fast-1", Name: "fixture_fast"}},
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "slow-1", Name: "fixture_slow"}},
		}},
		{Role: "user", Content: []llm.ContentBlock{
			{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "fast-1"}},
			{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "slow-1", IsError: true}},
		}},
	}}

	assert.False(t, toolResultCountsMatch(req, map[string]bt.ToolResultCount{
		"fixture_fast": {Total: 1},
	}), "an unexpected result must fail an exact-population expectation")
	assert.True(t, toolResultCountsMatch(req, map[string]bt.ToolResultCount{
		"fixture_fast": {Total: 1},
		"fixture_slow": {Total: 1, Errors: 1},
	}))
}
