package runner

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/memory"
)

// firstMsgText returns the joined text of a message's text-type content
// blocks (space-separated). Named distinctly from firstText
// (drain_inbox_test.go), which returns the first content block of a
// memory.Turn rather than an llm.Message.
//
// The Via annotation renders as its own leading text block ahead of the
// turn's original content (replay in loop_replay.go prepends rather than mutates),
// so this joins every text block rather than returning only the first —
// otherwise a check like "annotation present AND original text preserved"
// could never be expressed against a single block.
func firstMsgText(msg llm.Message) string {
	parts := make([]string, 0, len(msg.Content))
	for _, b := range msg.Content {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, " ")
}

// TestReplayRendersViaOnUserTurns proves replay renders memory.Turn.Via into
// the LLM context as a leading "(via ...)" annotation on user turns only —
// the annotation must not appear when Via is empty, and assistant turns are
// never annotated (Via is a user-turn concept: it names the surface a human
// injected feedback from).
func TestReplayRendersViaOnUserTurns(t *testing.T) {
	msgs, _, _ := replay([]memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "make a landing page"}}},
		{Index: 1, Role: "assistant", Via: "urn:ap:view:artifact:artifact-3f2a1b8c",
			Content: []memory.ContentBlock{{Type: "text", Text: "here you go"}}},
		{Index: 2, Role: "user", Via: "urn:ap:view:artifact:artifact-3f2a1b8c",
			Content: []memory.ContentBlock{{Type: "text", Text: "the CTA padding is wrong"}}},
	}, "ns/replay-fixture")
	require.Len(t, msgs, 3)

	// Turn 0 (no Via) is unchanged.
	assert.Equal(t, "make a landing page", firstMsgText(msgs[0]), "user turn without Via is not annotated")

	// Turn 1 is an assistant turn; Via is a user-turn concept and must never
	// be rendered onto assistant messages even if the field happened to be set.
	assert.Equal(t, "here you go", firstMsgText(msgs[1]), "assistant turn is never annotated with Via")

	// Turn 2 (Via) is prefixed with the human description so the agent knows
	// which artifact the feedback targets.
	assert.Contains(t, firstMsgText(msgs[2]), "the artifact view of artifact-3f2a1b8c", "user turn with Via is prefixed with the artifact description")
	assert.Contains(t, firstMsgText(msgs[2]), "the CTA padding is wrong", "original text is preserved after the annotation")
}

func TestReplay_SkipsRefusedTurnButAdvancesIndex(t *testing.T) {
	prior := []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "make a report"}}},
		{Index: 1, Role: "assistant", Refused: true, Content: []memory.ContentBlock{
			{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "toolu_x", Name: "artifact_prepare", Input: []byte(`{}`)}},
		}},
	}
	msgs, nextIndex, hadInitial := replay(prior, "ns/replay-fixture")

	require.Len(t, msgs, 1, "refused assistant turn must be excluded from replay")
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, 2, nextIndex, "nextIndex must still advance past the skipped turn")
	assert.True(t, hadInitial)
	// No dangling tool_use: the only message is the user turn, so the retry
	// re-generates the assistant turn cleanly.
	for _, m := range msgs {
		for _, b := range m.Content {
			assert.NotEqual(t, "tool_use", b.Type, "refused tool_use must not survive replay")
		}
	}
}

func TestReplay_TrailingRefusal_NudgesLastUserMessage(t *testing.T) {
	prior := []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "make a report"}}},
		{Index: 1, Role: "assistant", Refused: true, Content: []memory.ContentBlock{{Type: "text", Text: ""}}},
	}
	msgs, _, _ := replay(prior, "ns/replay-fixture")
	require.Len(t, msgs, 1)
	require.Equal(t, "user", msgs[len(msgs)-1].Role)
	joined := ""
	for _, b := range msgs[len(msgs)-1].Content {
		joined += b.Text
	}
	assert.Contains(t, joined, "content policy",
		"a trailing refusal must nudge the model on retry")
}

func TestReplay_NonTrailingRefusal_NoNudge(t *testing.T) {
	prior := []memory.Turn{
		{Index: 0, Role: "user", Content: []memory.ContentBlock{{Type: "text", Text: "make a report"}}},
		{Index: 1, Role: "assistant", Refused: true, Content: []memory.ContentBlock{{Type: "text", Text: ""}}},
		{Index: 2, Role: "assistant", Content: []memory.ContentBlock{{Type: "text", Text: "here you go"}}},
	}
	msgs, _, _ := replay(prior, "ns/replay-fixture")
	// Last message is the successful assistant turn; no nudge appended anywhere.
	for _, m := range msgs {
		for _, b := range m.Content {
			assert.NotContains(t, b.Text, "content policy")
		}
	}
}
