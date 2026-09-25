package runner_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	llmfake "github.com/authzed/openagentprimitives/pkg/agent/llm/fake"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// renderedLiveFakeTool answers with a fixed Result. Stateless StateImpact for
// the same reason blockingCancellableTool uses it: this fixture wires no
// SpiceDB client, and any other impact would deny fail-closed before Execute.
type renderedLiveFakeTool struct {
	name    string
	content string
	live    bool
}

func (f *renderedLiveFakeTool) Name() string                 { return f.name }
func (f *renderedLiveFakeTool) Kind() tool.Kind              { return tool.KindSandbox }
func (f *renderedLiveFakeTool) Description() string          { return "" }
func (f *renderedLiveFakeTool) InputSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (f *renderedLiveFakeTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Stateless}
}
func (f *renderedLiveFakeTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (f *renderedLiveFakeTool) Execute(context.Context, json.RawMessage, *tool.SessionContext) (tool.Result, error) {
	return tool.Result{Content: f.content, RenderedLive: f.live}, nil
}

// TestLoopPersistsRenderedLiveOntoTheToolResultBlock spans the seam no
// single-package test can see: the streaming toolkit stamps tool.Result, the
// transcript renderer reads memory.ToolResultBlock, and NOTHING between them
// fails to compile if the runner drops the field on the floor. Without this the
// two halves can each be green while a reload still shows nothing.
//
// Both directions in one run, from one tool batch, because that is the real
// shape: an agent calls the sub-agent toolkit and a memory query in the same
// turn, and exactly one of the two results belongs in the transcript.
func TestLoopPersistsRenderedLiveOntoTheToolResultBlock(t *testing.T) {
	const report = "status: success (7.3s, $0.09)\nno typos found"
	const rows = "id,tags\n1,alpha"

	// One assistant turn calling both tools, then agent_work_complete.
	batch := llmfake.Step{Resp: llm.Response{
		Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu_stream", Name: "codelike_subagent", Input: json.RawMessage(`{}`)}},
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "tu_plain", Name: "query_memory_fake", Input: json.RawMessage(`{}`)}},
		},
		StopReason: "tool_use", Usage: llm.Usage{InputTokens: 10, OutputTokens: 5},
	}}
	l, _, _, store := newLoopFixture(t, []llmfake.Step{
		batch,
		callStep("tu_done", "agent_work_complete", `{"summary":"done"}`),
	})
	l.Tools = append(meta.Load(),
		&renderedLiveFakeTool{name: "codelike_subagent", content: report, live: true},
		&renderedLiveFakeTool{name: "query_memory_fake", content: rows, live: false},
	)

	ctx := memory.WithSystemApproval(context.Background(), "test")
	require.NoError(t, l.Run(ctx))

	all, err := store.ReadAll(ctx)
	require.NoError(t, err)
	blocks := map[string]*memory.ToolResultBlock{}
	for _, tn := range all {
		for _, b := range tn.Content {
			if b.Type == "tool_result" && b.ToolResult != nil {
				blocks[b.ToolResult.ToolUseID] = b.ToolResult
			}
		}
	}

	streamed := blocks["tu_stream"]
	require.NotNil(t, streamed, "precondition: the streaming tool's result was recorded")
	assert.True(t, streamed.RenderedLive,
		"the runner must carry the tool's mark onto the persisted block, or a reload has nothing to render")
	unwrapped, ok := toolenvelope.Unwrap(streamed.Content)
	require.True(t, ok, "precondition: the persisted content is still the enveloped form the model saw")
	assert.Equal(t, report, unwrapped)

	plain := blocks["tu_plain"]
	require.NotNil(t, plain, "precondition: the ordinary tool's result was recorded")
	assert.False(t, plain.RenderedLive,
		"an ordinary tool result stays plumbing; marking it would flood the transcript")
}
