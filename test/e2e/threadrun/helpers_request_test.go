//go:build e2e

package threadrun

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// requestMentions backs expect.requestNotContains, whose claim is "this never
// reached the model". A value can arrive as prose, as a tool's input, or as a
// tool's output, in ANY earlier message — so a check that read only the
// latest message, or only text, would pass a leak that came in by another door.
func TestRequestMentionsSeesEveryMessageAndBlockKind(t *testing.T) {
	req := llm.Request{Messages: []llm.Message{
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "prose-needle"}}},
		{Role: "assistant", Content: []llm.ContentBlock{
			{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "u1", Name: "fixture_read", Input: json.RawMessage(`{"q":"input-needle"}`)}},
		}},
		{Role: "user", Content: []llm.ContentBlock{
			{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "u1", Content: "result-needle"}},
		}},
		{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "latest message"}}},
	}}

	for _, needle := range []string{"prose-needle", "input-needle", "result-needle", "latest message"} {
		assert.True(t, requestMentions(req, needle), "%q is in the request and must be found", needle)
	}
	assert.False(t, requestMentions(req, "absent-needle"), "a value in no message is not mentioned")
}
