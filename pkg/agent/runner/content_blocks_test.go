package runner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/llm/anthropic"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// TestContentBlocksFromMemory_DropsUnrenderableTypes is the R2-2 fix-round
// regression guard. pkg/channels/channelsd/pipeline/attachments.go writes a durable
// turn carrying a MemContent{Type:"attachment"} block (memory-side:
// ContentBlock{Type:"attachment", Attachment: &AttachmentBlock{...}}) plus a
// text manifest line in the SAME turn. Before the R2-2 fix, contentBlocksFromMemory
// passed the "attachment" block through verbatim — Type:"attachment",
// Text:"" — and every provider adapter's BuildParams/translate hard-errors on
// an unrecognized block type (anthropic.go: "unknown content block type
// %q"; openaicompat/translate.go: "unsupported ... content block %q"), so
// the FIRST LLM request on any attachment-bearing turn failed outright: not
// merely "the agent doesn't see the attachment" but "the turn is unusable".
//
// Since the native-attachment-passthrough pass, contentBlocksFromMemory no
// longer drops "attachment" itself — it carries the ref through in
// reference form, and the guarantee "no attachment ever reaches a provider
// adapter" moved one layer down, to hydrateAttachments, which runs
// immediately before every provider call. This test proves the guarantee
// still holds end to end at its new home: contentBlocksFromMemory's output
// still carries the ref (not silently re-dropping it, which would just be
// re-testing the old behavior), hydrateAttachments clears it, and
// anthropic.BuildParams accepts the hydrated result and produces a valid
// request.
func TestContentBlocksFromMemory_DropsUnrenderableTypes(t *testing.T) {
	in := []memory.ContentBlock{
		{Type: "text", Text: "[deck.pptx (application/vnd.openxmlformats-officedocument.presentationml.presentation, 12 pages) was attached and read. Its text is available at mem://ns/sess/inbound-asset/abc/text.txt — use fetch_artifact to read it.]"},
		{Type: "attachment", Attachment: &memory.AttachmentBlock{
			Filename: "deck.pptx",
			MIME:     "application/vnd.openxmlformats-officedocument.presentationml.presentation",
			Ref:      "mem://ns/sess/inbound-asset/abc/raw/deck.pptx",
			TextRef:  "mem://ns/sess/inbound-asset/abc/text.txt",
			Pages:    12,
		}},
	}

	out := contentBlocksFromMemory(in)
	require.Len(t, out, 2, "contentBlocksFromMemory now carries the attachment through in reference form rather than dropping it — the manifest text block plus the attachment ref")
	require.Equal(t, "attachment", out[1].Type)
	require.NotNil(t, out[1].Attachment, "the reference form must survive the memory->llm conversion")

	l := &Loop{}
	msgs := []llm.Message{{Role: "user", Content: out}}
	out = l.hydrateAttachments(context.Background(), msgs)[0].Content

	require.Len(t, out, 1, "hydrateAttachments must drop the attachment ref; only the text manifest block survives")
	for _, b := range out {
		assert.Contains(t, []string{"text", "tool_use", "tool_result", "container_upload"}, b.Type,
			"no block reaching a provider adapter may be a type it doesn't render")
	}
	assert.Equal(t, "text", out[0].Type)
	assert.Contains(t, out[0].Text, "fetch_artifact", "the manifest line (the agent's only path to the handle) must survive")

	params, err := anthropic.BuildParams(llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Messages:  []llm.Message{{Role: "user", Content: out}},
	})
	require.NoError(t, err, "an attachment-bearing turn must produce a valid LLM request, not an 'unknown content block type' error")
	require.Len(t, params.Messages, 1)
	require.Len(t, params.Messages[0].Content, 1)
	require.NotNil(t, params.Messages[0].Content[0].OfText, "the surviving block must be a real text content block")
}

// TestContentBlocksFromMemory_AllUnrenderable_FallsBackToPlaceholderText
// covers the defensive empty-turn guard: if a turn's content collapses to
// nothing renderable, the result must never be an empty Content slice,
// which is itself a provider error distinct from "unknown block type".
//
// The turn used here — a single attachment with no accompanying text (not
// the shape pkg/channels/channelsd/pipeline/attachments.go actually writes today, but
// a future caller might not guarantee a manifest line) — is also the exact
// shape that exposed a real defect in the native-attachment-passthrough
// pass: contentBlocksFromMemory keeps the attachment in reference form
// (its own empty-turn fallback never fires, since the output isn't empty),
// so the guard that used to live here does nothing; hydrateAttachments then
// clears the ref, which — unguarded — would leave the message with zero
// blocks. hydrateAttachments carries its own placeholder fallback for
// exactly this case (see attachments.go and
// TestHydrateAttachments_AttachmentOnlyMessage_FallsBackToPlaceholder for
// the direct-unit-level version of this guard); this test proves the same
// guarantee holds when driven through the full memory->llm->hydrate->provider
// pipeline, which is the path production code actually takes.
func TestContentBlocksFromMemory_AllUnrenderable_FallsBackToPlaceholderText(t *testing.T) {
	in := []memory.ContentBlock{
		{Type: "attachment", Attachment: &memory.AttachmentBlock{Filename: "orphan.pdf", MIME: "application/pdf", Ref: "ref-1"}},
	}

	out := contentBlocksFromMemory(in)
	require.Len(t, out, 1, "attachment is renderable (in reference form) in contentBlocksFromMemory now, so its own empty-turn fallback does not fire here")
	require.Equal(t, "attachment", out[0].Type, "this turn's only block is still an unresolved ref at this point in the pipeline")

	l := &Loop{}
	msgs := []llm.Message{{Role: "user", Content: out}}
	out = l.hydrateAttachments(context.Background(), msgs)[0].Content

	require.Len(t, out, 1, "an empty result would itself be a provider error — hydrateAttachments must fall back to a placeholder rather than leave the message empty")
	assert.Equal(t, "text", out[0].Type)
	assert.NotEmpty(t, out[0].Text)

	params, err := anthropic.BuildParams(llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Messages:  []llm.Message{{Role: "user", Content: out}},
	})
	require.NoError(t, err, "the placeholder fallback must itself produce a valid LLM request")
	require.Len(t, params.Messages[0].Content, 1)
}

// TestContentBlocksFromMemory_PreservesKnownTypes is a control: ordinary
// text/tool_use/tool_result turns (the vast majority of every session's
// transcript) must round-trip exactly as before this fix — the filter must
// not accidentally drop anything it used to carry through.
func TestContentBlocksFromMemory_PreservesKnownTypes(t *testing.T) {
	in := []memory.ContentBlock{
		{Type: "text", Text: "hello"},
		{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "t1", Name: "some_tool", Input: []byte(`{"a":1}`)}},
		{Type: "tool_result", ToolResult: &memory.ToolResultBlock{ToolUseID: "t1", Content: "ok"}},
	}
	out := contentBlocksFromMemory(in)
	require.Len(t, out, 3)
	assert.Equal(t, "text", out[0].Type)
	assert.Equal(t, "tool_use", out[1].Type)
	require.NotNil(t, out[1].ToolUse)
	assert.Equal(t, "some_tool", out[1].ToolUse.Name)
	assert.Equal(t, "tool_result", out[2].Type)
	require.NotNil(t, out[2].ToolResult)
	assert.Equal(t, "ok", out[2].ToolResult.Content)
}

// TestContentBlocksFromMemory_UnknownFutureType_FallsBackToPlaceholder
// restores direct coverage of contentBlocksFromMemory's OWN empty-turn
// fallback, which lost its only driver when "attachment" became renderable
// there.
//
// The branch is not dead: it is the fail-safe for a memory.ContentBlock type
// added later and not yet taught to any provider adapter. The allowlist drops
// such a block deliberately (better dropped than passed to an adapter that
// hard-errors on it), and if it was the turn's ONLY block, the result would be
// an empty Content slice — itself a distinct provider error. This drives that
// path with a type no adapter will ever know, which is the only shape that can
// still reach it.
func TestContentBlocksFromMemory_UnknownFutureType_FallsBackToPlaceholder(t *testing.T) {
	in := []memory.ContentBlock{{Type: "some-future-block-type", Text: "opaque"}}

	out := contentBlocksFromMemory(in)

	require.Len(t, out, 1, "an empty result would itself be a provider error")
	assert.Equal(t, "text", out[0].Type)
	assert.Equal(t, emptyContentPlaceholderText, out[0].Text,
		"the placeholder specifically, not merely some non-empty text")

	params, err := anthropic.BuildParams(llm.Request{
		Model:     "claude-opus-4-8",
		MaxTokens: 16,
		Messages:  []llm.Message{{Role: "user", Content: out}},
	})
	require.NoError(t, err, "the placeholder fallback must itself produce a valid LLM request")
	require.Len(t, params.Messages[0].Content, 1)
}
