//go:build e2e

package e2e_test

import (
	"encoding/json"
	"testing"
	"time"

	core "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/test/e2e"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Successful mutations must traverse the actual runner gates, rather than only
// testing the HTTP endpoint. The script captures the server-selected domain and
// goal ID from tool results, exactly as a real model must do.
func TestGoalsManageThroughRunnerPipeline(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: "bronzethread/testdata/agent-goaldemo", DefaultTimeout: 30 * time.Second})
	h.WaitForAgentClassValid("goaldemo", 30*time.Second)
	h.WaitForAuthzSchema(60 * time.Second)
	var resource string
	var created core.Goal
	result := func(v any) core.Response {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		var r core.Response
		require.NoError(t, json.Unmarshal(b, &r))
		return r
	}
	createArgs := func() map[string]any {
		return map[string]any{"resource": resource, "requestID": "workflow-create", "title": "Meeting agenda", "outcome": "Prepare the agenda"}
	}
	h.LLM.OnUserMessage("goal workflow").Reply(e2e.ToolUse("list_goals", map[string]any{}))
	h.LLM.OnToolResult("list_goals", func(v any) bool {
		r := result(v)
		resource = r.Resource
		require.NotEmpty(t, resource)
		require.NotNil(t, r.Page)
		assert.Empty(t, r.Page.Goals)
		assert.False(t, r.ExecutionAvailable)
		return true
	}).ReplyFn(func() []e2e.ReplyPart { return []e2e.ReplyPart{e2e.ToolUse("create_goal", createArgs())} })
	h.LLM.OnToolResult("create_goal", func(v any) bool {
		r := result(v)
		require.NotNil(t, r.Goal)
		created = *r.Goal
		assert.Equal(t, core.Draft, created.State)
		assert.Equal(t, int64(1), created.Revision)
		return true
	}).ReplyFn(func() []e2e.ReplyPart { return []e2e.ReplyPart{e2e.ToolUse("create_goal", createArgs())} })
	h.LLM.OnToolResult("create_goal", func(v any) bool {
		r := result(v)
		require.NotNil(t, r.Goal)
		assert.Equal(t, created, *r.Goal)
		return true
	}).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("update_goal", map[string]any{"resource": resource, "id": created.ID, "revision": 1, "requestID": "workflow-activate", "action": "activate"})}
	})
	h.LLM.OnToolResult("update_goal", func(v any) bool {
		r := result(v)
		require.NotNil(t, r.Goal)
		assert.Equal(t, core.Active, r.Goal.State)
		assert.Equal(t, int64(2), r.Goal.Revision)
		return true
	}).ReplyFn(func() []e2e.ReplyPart {
		return []e2e.ReplyPart{e2e.ToolUse("get_goal", map[string]any{"id": created.ID})}
	})
	h.LLM.OnToolResult("get_goal", func(v any) bool {
		r := result(v)
		require.NotNil(t, r.Goal)
		assert.Equal(t, created.ID, r.Goal.ID)
		assert.Equal(t, int64(2), r.Goal.Revision)
		assert.False(t, r.ExecutionAvailable)
		return true
	}).Reply(e2e.RespondToUser("Goal workflow verified."))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "goal workflow verified"}))
	h.SendUserMessage("Run the goal workflow")
	h.ExpectAgentReply(e2e.Contains("Goal workflow verified."))
	ns, name := h.SessionRef()
	h.WaitForSessionPhase(ns, name, "Idle", h.DefaultTimeout())
	h.LLM.AssertAllRulesConsumed()
}
