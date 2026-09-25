package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildParams(t *testing.T) {
	req := llm.Request{
		Model:     "gpt-5.3-codex",
		MaxTokens: 4096,
		System:    []llm.SystemBlock{{Text: "sys-a"}, {Text: "sys-b", Cacheable: true}},
		Tools: []llm.ToolDef{{
			Name:        "get_weather",
			Description: "Get weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"loc":{"type":"string"}},"required":["loc"]}`),
			Cacheable:   true, // must be ignored (no-op) — no error, no panic
		}},
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hi"}}},
			{Role: "assistant", Content: []llm.ContentBlock{
				{Type: "text", Text: "let me check"},
				{Type: "tool_use", ToolUse: &llm.ToolUseBlock{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{"loc":"NYC"}`)}},
			}},
			{Role: "user", Content: []llm.ContentBlock{
				{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "call_1", Content: "sunny"}},
			}},
		},
	}

	params, err := BuildParams(req)
	require.NoError(t, err)

	assert.Equal(t, "gpt-5.3-codex", string(params.Model))
	assert.EqualValues(t, 4096, params.MaxCompletionTokens.Value)
	require.Len(t, params.Tools, 1)

	// Messages: system (joined), user text, assistant(text+tool_calls), tool result.
	require.Len(t, params.Messages, 4)

	// System block is first and joins both system texts.
	require.NotNil(t, params.Messages[0].OfSystem)

	// Assistant message carries exactly one tool call mapped from the tool_use block.
	asst := params.Messages[2].OfAssistant
	require.NotNil(t, asst)
	require.Len(t, asst.ToolCalls, 1)
	require.NotNil(t, asst.ToolCalls[0].OfFunction)
	assert.Equal(t, "call_1", asst.ToolCalls[0].OfFunction.ID)
	assert.Equal(t, "get_weather", asst.ToolCalls[0].OfFunction.Function.Name)
	assert.JSONEq(t, `{"loc":"NYC"}`, asst.ToolCalls[0].OfFunction.Function.Arguments)

	// Tool result becomes a tool-role message.
	require.NotNil(t, params.Messages[3].OfTool)
}

func TestBuildParams_ServerToolRejected(t *testing.T) {
	_, err := BuildParams(llm.Request{
		Model: "gpt-5.3-codex",
		Tools: []llm.ToolDef{{Name: "web_search", ServerType: "web_search_20250305"}},
	})
	require.Error(t, err) // OpenAI has no equivalent; do not silently drop it.
}

// TestBuildParams_NativeBlocksRejected covers both native block types this
// provider cannot emit. A model row declaring a MIME that resolves to one of
// these must never reach here for an OpenAI-compat provider (see
// TestOnlyAdaptersThatEmitNativeBlocksDeclareThem in pkg/agent/llm/models),
// but if it ever does, BuildParams must error rather than silently drop the
// file — the agent would otherwise answer about a document it never received.
func TestBuildParams_NativeBlocksRejected(t *testing.T) {
	cases := []struct {
		name  string
		block llm.ContentBlock
	}{
		{name: "document block", block: llm.ContentBlock{Type: "document", MIME: "application/pdf", Data: []byte("%PDF-1.7")}},
		{name: "image block", block: llm.ContentBlock{Type: "image", MIME: "image/png", Data: []byte("PNGBYTES")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildParams(llm.Request{
				Model:    "gpt-5.3-codex",
				Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{tc.block}}},
			})
			require.Error(t, err, "native block must not be silently dropped")
			assert.Contains(t, err.Error(), tc.block.Type, "error must name the rejected block type")
			assert.Contains(t, err.Error(), "NativeInputMIMEs",
				"error must point at the fix (the model row's NativeInputMIMEs), not just a generic unsupported-block message")
		})
	}
}

// TestBuildParams_UserContentOrderPreserved covers a user message that mixes
// text and tool_result blocks: [text, tool_result, text]. The emitted OpenAI
// messages must follow the neutral Content encounter order — text, then the
// tool message, then a second text message — not tool-messages-first with
// the joined text appended at the end.
func TestBuildParams_UserContentOrderPreserved(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.3-codex",
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{
				{Type: "text", Text: "first"},
				{Type: "tool_result", ToolResult: &llm.ToolResultBlock{ToolUseID: "call_1", Content: "sunny"}},
				{Type: "text", Text: "second"},
			}},
		},
	}

	params, err := BuildParams(req)
	require.NoError(t, err)

	require.Len(t, params.Messages, 3)
	require.NotNil(t, params.Messages[0].OfUser)
	assert.Equal(t, "first", params.Messages[0].OfUser.Content.OfString.Value)
	require.NotNil(t, params.Messages[1].OfTool)
	require.NotNil(t, params.Messages[2].OfUser)
	assert.Equal(t, "second", params.Messages[2].OfUser.Content.OfString.Value)
}

// TestBuildParams_UserTextBlocksJoinedWithBlankLine covers two adjacent text
// blocks within a single user message: they must join with "\n\n", matching
// the separator already used for system blocks.
func TestBuildParams_UserTextBlocksJoinedWithBlankLine(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.3-codex",
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{
				{Type: "text", Text: "part-a"},
				{Type: "text", Text: "part-b"},
			}},
		},
	}

	params, err := BuildParams(req)
	require.NoError(t, err)

	require.Len(t, params.Messages, 1)
	require.NotNil(t, params.Messages[0].OfUser)
	assert.Equal(t, "part-a\n\npart-b", params.Messages[0].OfUser.Content.OfString.Value)
}

// TestBuildParams_AssistantTextBlocksJoinedWithBlankLine mirrors the above
// for the assistant branch.
func TestBuildParams_AssistantTextBlocksJoinedWithBlankLine(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.3-codex",
		Messages: []llm.Message{
			{Role: "assistant", Content: []llm.ContentBlock{
				{Type: "text", Text: "part-a"},
				{Type: "text", Text: "part-b"},
			}},
		},
	}

	params, err := BuildParams(req)
	require.NoError(t, err)

	require.Len(t, params.Messages, 1)
	asst := params.Messages[0].OfAssistant
	require.NotNil(t, asst)
	assert.Equal(t, "part-a\n\npart-b", asst.Content.OfString.Value)
}

// TestBuildParams_SingleTextBlockNoTrailingSeparator guards the common
// single-block case: no separator should leak in when there's only one block.
func TestBuildParams_SingleTextBlockNoTrailingSeparator(t *testing.T) {
	req := llm.Request{
		Model: "gpt-5.3-codex",
		Messages: []llm.Message{
			{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "solo"}}},
		},
	}

	params, err := BuildParams(req)
	require.NoError(t, err)

	require.Len(t, params.Messages, 1)
	require.NotNil(t, params.Messages[0].OfUser)
	assert.Equal(t, "solo", params.Messages[0].OfUser.Content.OfString.Value)
}

// TestBuildParams_CacheableHintIsInert locks the deliberate no-op: OpenAI has
// no client-side cache marker, so a Cacheable block must serialize identically
// to an unmarked one, and no Anthropic-shaped cache_control may leak into the
// request body. Table-driven over block type because BuildParams branches per
// type — text accumulates into a joined string, tool_use goes through the
// assistant tool-calls array, tool_result goes through oa.ToolMessage — so a
// leak added to only one branch (e.g. markCacheBreakpoints in the runner marks
// the last block of the last message, which after a tool-call turn is a
// tool_result) would stay hidden behind a text-only case.
func TestBuildParams_CacheableHintIsInert(t *testing.T) {
	cases := []struct {
		name  string
		role  string
		block llm.ContentBlock
	}{
		{
			name:  "text block",
			role:  "user",
			block: llm.ContentBlock{Type: "text", Text: "hello"},
		},
		{
			name: "tool_use block",
			role: "assistant",
			block: llm.ContentBlock{
				Type:    "tool_use",
				ToolUse: &llm.ToolUseBlock{ID: "call_1", Name: "get_weather", Input: json.RawMessage(`{}`)},
			},
		},
		{
			name: "tool_result block",
			role: "user",
			block: llm.ContentBlock{
				Type:       "tool_result",
				ToolResult: &llm.ToolResultBlock{ToolUseID: "call_1", Content: "sunny"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plainBlock := tc.block
			markedBlock := tc.block
			markedBlock.Cacheable = true

			marked := llm.Request{
				Model:     "gpt-5.4",
				MaxTokens: 1024,
				Messages:  []llm.Message{{Role: tc.role, Content: []llm.ContentBlock{markedBlock}}},
			}
			plain := marked
			plain.Messages = []llm.Message{{Role: tc.role, Content: []llm.ContentBlock{plainBlock}}}

			withMark, err := BuildParams(marked)
			require.NoError(t, err)
			withoutMark, err := BuildParams(plain)
			require.NoError(t, err)

			gotMarked, err := json.Marshal(withMark)
			require.NoError(t, err)
			gotPlain, err := json.Marshal(withoutMark)
			require.NoError(t, err)

			assert.JSONEq(t, string(gotPlain), string(gotMarked),
				"Cacheable is a deliberate no-op on the OpenAI path: the marked and "+
					"unmarked requests must serialize identically")
			assert.NotContains(t, string(gotMarked), "cache_control",
				"no Anthropic-shaped cache_control may leak into an OpenAI request body")
		})
	}
}
