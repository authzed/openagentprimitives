package anthropic_test

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
)

func TestBuildParams_MarkedMessageBlockGetsCacheControl(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		Messages: []llm.Message{{
			Role: "user",
			Content: []llm.ContentBlock{
				{Type: "text", Text: "first"},
				{Type: "text", Text: "second", Cacheable: true},
			},
		}},
	}

	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed for a well-formed request")

	require.Len(t, params.Messages, 1)
	blocks := params.Messages[0].Content
	require.Len(t, blocks, 2)

	require.NotNil(t, blocks[0].OfText)
	assert.True(t, param.IsOmitted(blocks[0].OfText.CacheControl),
		"an unmarked block must not carry cache_control")

	require.NotNil(t, blocks[1].OfText)
	assert.False(t, param.IsOmitted(blocks[1].OfText.CacheControl),
		"the marked block must carry an ephemeral cache_control")
}

func TestBuildParams_UnmarkedRequestHasNoMessageCacheControl(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		Messages: []llm.Message{{
			Role:    "user",
			Content: []llm.ContentBlock{{Type: "text", Text: "hello"}},
		}},
	}

	params, err := anthropic.BuildParams(req)
	require.NoError(t, err)

	require.Len(t, params.Messages, 1)
	require.Len(t, params.Messages[0].Content, 1)
	require.NotNil(t, params.Messages[0].Content[0].OfText)
	assert.True(t, param.IsOmitted(params.Messages[0].Content[0].OfText.CacheControl),
		"no marks in means no cache_control out — this is the pre-change behavior")
}

// TestBuildParams_MarkedMessageBlockGetsCacheControlByBlockType exercises
// applyMessageCacheControl's three branches independently. Shared shape: a
// single Cacheable block of the given type, in a message of the given role;
// only the block-building and expected populated union field vary. Each
// case asserts both that the matching field carries the breakpoint and that
// the other two do not, so a case in applyMessageCacheControl's switch that
// writes to the wrong sibling field (a copy-paste typo between OfToolUse and
// OfToolResult, say) fails here instead of shipping unnoticed.
func TestBuildParams_MarkedMessageBlockGetsCacheControlByBlockType(t *testing.T) {
	cases := []struct {
		name           string
		role           string
		block          llm.ContentBlock
		wantText       bool
		wantToolUse    bool
		wantToolResult bool
	}{
		{
			name:     "text block: OfText carries cache_control, others nil",
			role:     "user",
			block:    llm.ContentBlock{Type: "text", Text: "hi", Cacheable: true},
			wantText: true,
		},
		{
			name: "tool_use block: OfToolUse carries cache_control, others nil",
			role: "assistant",
			block: llm.ContentBlock{
				Type:      "tool_use",
				ToolUse:   &llm.ToolUseBlock{ID: "t1", Name: "get_weather", Input: json.RawMessage(`{}`)},
				Cacheable: true,
			},
			wantToolUse: true,
		},
		{
			name: "tool_result block: OfToolResult carries cache_control, others nil",
			role: "user",
			block: llm.ContentBlock{
				Type:       "tool_result",
				ToolResult: &llm.ToolResultBlock{ToolUseID: "t1", Content: "ok"},
				Cacheable:  true,
			},
			wantToolResult: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := llm.Request{
				Model:     "claude-opus-5",
				MaxTokens: 1024,
				Messages: []llm.Message{{
					Role:    tc.role,
					Content: []llm.ContentBlock{tc.block},
				}},
			}

			params, err := anthropic.BuildParams(req)
			require.NoError(t, err, "BuildParams must succeed")
			require.Len(t, params.Messages, 1)
			require.Len(t, params.Messages[0].Content, 1)
			got := params.Messages[0].Content[0]

			assertCacheControlOn(t, got, tc.wantText, tc.wantToolUse, tc.wantToolResult)
		})
	}
}

// assertCacheControlOn checks that exactly the requested union variant is
// populated and carries a stamped cache_control, and that the other two
// variants are absent — pinning down which SDK field the breakpoint landed
// on, not just that some field did.
func assertCacheControlOn(t *testing.T, got sdk.ContentBlockParamUnion, wantText, wantToolUse, wantToolResult bool) {
	t.Helper()

	if wantText {
		require.NotNil(t, got.OfText, "OfText must be set")
		assert.False(t, param.IsOmitted(got.OfText.CacheControl), "OfText.CacheControl must be stamped")
	} else {
		assert.Nil(t, got.OfText, "OfText must not be set")
	}

	if wantToolUse {
		require.NotNil(t, got.OfToolUse, "OfToolUse must be set")
		assert.False(t, param.IsOmitted(got.OfToolUse.CacheControl), "OfToolUse.CacheControl must be stamped")
	} else {
		assert.Nil(t, got.OfToolUse, "OfToolUse must not be set")
	}

	if wantToolResult {
		require.NotNil(t, got.OfToolResult, "OfToolResult must be set")
		assert.False(t, param.IsOmitted(got.OfToolResult.CacheControl), "OfToolResult.CacheControl must be stamped")
	} else {
		assert.Nil(t, got.OfToolResult, "OfToolResult must not be set")
	}
}

// TestBuildParams_FourBreakpointBudgetExhaustedExactly builds one request
// exercising all four breakpoint sources this repo uses together — the
// system block, the last tool definition, and the two message-content
// blocks the runner's markCacheBreakpoints marks (anchor + moving) — and
// counts "cache_control" occurrences in the MARSHALLED request body rather
// than asserting on Go structs. The per-block-type tests above (and
// TestBuildParamsRoundTrip in anthropic_test.go) check param.IsOmitted,
// which confirms a field was SET but not that it survives `omitzero`
// marshaling; a field set on the wrong nested type, or a struct compared by
// value against a zero sentinel, can pass IsOmitted and still never reach
// Anthropic in the request body. No individual function in this package
// counts the total across all four sources — this test is that count, and
// it doubles as a regression guard for the 4-breakpoint budget itself: a
// fifth mark anywhere would fail Anthropic's request with a hard 400 on
// every session past the second turn.
func TestBuildParams_FourBreakpointBudgetExhaustedExactly(t *testing.T) {
	req := llm.Request{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		System:    []llm.SystemBlock{{Text: "system prompt", Cacheable: true}},
		Tools: []llm.ToolDef{
			{Name: "first_tool", Description: "d", InputSchema: []byte(`{"type":"object"}`)},
			{Name: "last_tool", Description: "d", InputSchema: []byte(`{"type":"object"}`), Cacheable: true},
		},
		Messages: []llm.Message{
			{
				Role:    "user",
				Content: []llm.ContentBlock{{Type: "text", Text: "anchor", Cacheable: true}},
			},
			{
				Role:    "assistant",
				Content: []llm.ContentBlock{{Type: "text", Text: "reply"}},
			},
			{
				Role:    "user",
				Content: []llm.ContentBlock{{Type: "text", Text: "moving", Cacheable: true}},
			},
		},
	}

	params, err := anthropic.BuildParams(req)
	require.NoError(t, err, "BuildParams must succeed for a well-formed request")

	body, err := json.Marshal(params)
	require.NoError(t, err, "marshal full request params")

	assert.Equal(t, 4, strings.Count(string(body), `"cache_control"`),
		"the four-breakpoint budget is exactly exhausted: system, last tool, anchor, moving")
}
