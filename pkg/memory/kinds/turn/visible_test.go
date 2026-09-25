package turn_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// textTurn is defined in accessor_test.go (same test package).

// respondTurn builds an assistant turn whose reply is delivered via the
// respond_to_user meta tool (a tool_use block with the text in input JSON) —
// the real shape an agent reply takes, not a text block.
func respondTurn(index int, text string) memory.Turn {
	return memory.Turn{
		Index: index,
		Role:  "assistant",
		Content: []memory.ContentBlock{{
			Type:    "tool_use",
			ToolUse: &memory.ToolUseBlock{ID: "t1", Name: "respond_to_user", Input: []byte(`{"text":` + quote(text) + `}`)},
		}},
		CreatedAt: time.Unix(int64(index), 0).UTC(),
	}
}

func quote(s string) string { return `"` + s + `"` }

// respondTurnWithPreamble builds an assistant turn that emits model "preamble"
// text blocks AND the respond_to_user tool_use — the shape the scripted LLM
// and real runners produce. Live surfaces show ONLY the respond_to_user reply.
func respondTurnWithPreamble(index int, preamble, reply string) memory.Turn {
	return memory.Turn{
		Index: index,
		Role:  "assistant",
		Content: []memory.ContentBlock{
			{Type: "text", Text: preamble},
			{Type: "text", Text: reply},
			{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "t1", Name: "respond_to_user", Input: []byte(`{"text":` + quote(reply) + `}`)}},
		},
		CreatedAt: time.Unix(int64(index), 0).UTC(),
	}
}

// inboxDone is the marker the runner writes at a drained inbox turn's index.
func inboxDone(index int) memory.Turn {
	return memory.Turn{
		Index:     index,
		Role:      "inbox_done",
		Content:   []memory.ContentBlock{{Type: "text", Text: "inbox entry consumed"}},
		CreatedAt: time.Unix(int64(index), 0).UTC(),
	}
}

func texts(msgs []turn.VisibleMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ":" + m.Text
	}
	return out
}

func TestVisibleMessages(t *testing.T) {
	cases := []struct {
		name  string
		turns []memory.Turn
		want  []string
	}{
		{
			name: "drained inbox turn is not double-counted (the reported bug)",
			// A mid-session message lands as an "inbox" turn, which the runner
			// promotes to a "user" turn at a new index and marks "inbox_done" at
			// the inbox index. All three survive in memory; only ONE user bubble
			// should render.
			turns: []memory.Turn{
				textTurn(0, "user", "what time of the day is it?"),
				respondTurn(1, "Arr, the tides, not the hour!"),
				textTurn(2, "inbox", "how many milliseconds to the full moon?"),
				inboxDone(2),
				textTurn(3, "user", "how many milliseconds to the full moon?"),
				respondTurn(4, "Arr, no charts fer that, matey!"),
			},
			want: []string{
				"user:what time of the day is it?",
				"agent:Arr, the tides, not the hour!",
				"user:how many milliseconds to the full moon?",
				"agent:Arr, no charts fer that, matey!",
			},
		},
		{
			name: "unconsumed inbox turn IS shown (sent, not yet drained)",
			// No inbox_done marker and no promoted user turn yet: the message is
			// pending in the runner's inbox but must still render for the sender.
			turns: []memory.Turn{
				textTurn(0, "user", "first"),
				respondTurn(1, "reply"),
				textTurn(2, "inbox", "just sent, runner parked"),
			},
			want: []string{"user:first", "agent:reply", "user:just sent, runner parked"},
		},
		{
			name:  "first message via spec.Prompt (plain user turn) is not duplicated",
			turns: []memory.Turn{textTurn(0, "user", "hello"), respondTurn(1, "hi")},
			want:  []string{"user:hello", "agent:hi"},
		},
		{
			name: "input need not be sorted",
			turns: []memory.Turn{
				respondTurn(4, "Arr, no charts!"),
				textTurn(0, "user", "what time?"),
				textTurn(3, "user", "how many ms?"),
				inboxDone(2),
				respondTurn(1, "the tides!"),
				textTurn(2, "inbox", "how many ms?"),
			},
			want: []string{"user:what time?", "agent:the tides!", "user:how many ms?", "agent:Arr, no charts!"},
		},
		{
			name: "assistant text block (no respond_to_user) is shown",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				textTurn(1, "assistant", "plain reply"),
			},
			want: []string{"user:q", "agent:plain reply"},
		},
		{
			name: "tool-only assistant turn (no user-facing text) is dropped",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				{Index: 1, Role: "assistant", Content: []memory.ContentBlock{{
					Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "x", Name: "query_memory", Input: []byte(`{}`)},
				}}},
				respondTurn(2, "the real answer"),
			},
			want: []string{"user:q", "agent:the real answer"},
		},
		{
			name: "internal tool-calling turn's preamble text is dropped (not shown as a reply)",
			// An assistant turn that makes an internal (non-respond_to_user) tool
			// call also carries the model's preamble text. Live surfaces show only
			// a transient "thinking…" caption + the final respond_to_user reply and
			// discard this preamble; a resumed transcript must match — the preamble
			// is internal reasoning, not an agent message.
			turns: []memory.Turn{
				textTurn(0, "user", "what's the weather?"),
				{Index: 1, Role: "assistant", Content: []memory.ContentBlock{
					{Type: "text", Text: "let me look that up for you"},
					{Type: "tool_use", ToolUse: &memory.ToolUseBlock{ID: "x", Name: "get_weather", Input: []byte(`{}`)}},
				}},
				respondTurn(2, "it is sunny"),
			},
			want: []string{"user:what's the weather?", "agent:it is sunny"},
		},
		{
			name: "non-conversational roles are skipped",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				textTurn(1, "system_note", "seatbelt engaged"),
				respondTurn(2, "a"),
			},
			want: []string{"user:q", "agent:a"},
		},
		{
			name: "whitespace-only turns are dropped",
			turns: []memory.Turn{
				textTurn(0, "user", "   "),
				textTurn(1, "user", "real"),
			},
			want: []string{"user:real"},
		},
		{name: "empty input", turns: nil, want: []string{}},
		{
			name: "respond_to_user is authoritative: preamble text blocks are dropped",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				respondTurnWithPreamble(1, "let me think about this", "the answer is 42"),
			},
			// Live showed only "the answer is 42"; the resumed transcript must too
			// (not the preamble, and not the doubled respond_to_user text block).
			want: []string{"user:q", "agent:the answer is 42"},
		},
		{
			name: "assistant text block with NO respond_to_user is still the reply",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				textTurn(1, "assistant", "a plain streamed reply"),
			},
			want: []string{"user:q", "agent:a plain streamed reply"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := turn.VisibleMessages(tc.turns)
			assert.Equal(t, tc.want, texts(got))
		})
	}
}

// TestVisibleMessagesCreatedAt confirms the promoted-user copy's timestamp is
// carried through (callers render it), independent of the dropped inbox turn.
func TestVisibleMessagesCreatedAt(t *testing.T) {
	got := turn.VisibleMessages([]memory.Turn{
		textTurn(2, "inbox", "hi"),
		inboxDone(2),
		textTurn(3, "user", "hi"),
	})
	require.Len(t, got, 1)
	assert.Equal(t, time.Unix(3, 0).UTC(), got[0].CreatedAt)
}

func TestVisibleMessageCarriesVia(t *testing.T) {
	msgs := turn.VisibleMessages([]memory.Turn{
		{Index: 0, Role: "user", Via: "urn:ap:view:artifact:artifact-3f2a1b8c",
			Content: []memory.ContentBlock{{Type: "text", Text: "hi"}}},
	})
	require.Len(t, msgs, 1)
	assert.Equal(t, "urn:ap:view:artifact:artifact-3f2a1b8c", msgs[0].Via)
}

// planNoteTurn builds a "plans" system_note turn as the runner persists it:
// role "system_note", content a text block of the wrapped {kind,data} JSON.
func planNoteTurn(index int, op, name, itemsJSON string) memory.Turn {
	var data string
	if op == "delete" {
		data = `{"op":"delete","name":` + quote(name) + `}`
	} else {
		data = `{"op":"upsert","plan":{"name":` + quote(name) + `,"items":` + itemsJSON + `,"updated_at":"2026-01-01T00:00:00Z"}}`
	}
	wrapped := `{"kind":"plans","v":1,"data":` + data + `}`
	return memory.Turn{
		Index:     index,
		Role:      "system_note",
		Content:   []memory.ContentBlock{{Type: "text", Text: wrapped}},
		CreatedAt: time.Unix(int64(index), 0).UTC(),
	}
}

func timelineKinds(items []turn.TimelineItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		if it.Kind == "plan" {
			del := ""
			if it.Plan.Deleted {
				del = "(deleted)"
			}
			out[i] = "plan:" + it.Plan.PlanName + del
		} else {
			out[i] = it.Message.Role + ":" + it.Message.Text
		}
	}
	return out
}

func TestVisibleTimeline(t *testing.T) {
	itemsV1 := `[{"id":"a","label":"pack","status":"in_progress"}]`
	itemsV2 := `[{"id":"a","label":"pack","status":"done"}]`
	cases := []struct {
		name  string
		turns []memory.Turn
		want  []string
	}{
		{
			name: "one plan card per name at first appearance, latest snapshot",
			// plan(trip) upsert, a message, plan(trip) upsert again (across the msg).
			turns: []memory.Turn{
				textTurn(0, "user", "start"),
				planNoteTurn(1, "upsert", "trip", itemsV1),
				respondTurn(2, "packing"),
				planNoteTurn(3, "upsert", "trip", itemsV2),
			},
			// One "trip" card at its first-appearance index (1), between the user
			// message and the reply. The second upsert updates it in place.
			want: []string{"user:start", "plan:trip", "agent:packing"},
		},
		{
			name: "two distinct plan names -> two cards in order",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				planNoteTurn(1, "upsert", "alpha", itemsV1),
				planNoteTurn(2, "upsert", "beta", itemsV1),
			},
			want: []string{"user:q", "plan:alpha", "plan:beta"},
		},
		{
			name: "final op delete marks the card Deleted",
			turns: []memory.Turn{
				textTurn(0, "user", "q"),
				planNoteTurn(1, "upsert", "trip", itemsV1),
				planNoteTurn(2, "delete", "trip", ""),
			},
			want: []string{"user:q", "plan:trip(deleted)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, timelineKinds(turn.VisibleTimeline(tc.turns)))
		})
	}
}

// TestVisibleTimelinePlanSnapshotIsLatest confirms the carried snapshot is the
// final upsert (final item status), not the first.
func TestVisibleTimelinePlanSnapshotIsLatest(t *testing.T) {
	items := turn.VisibleTimeline([]memory.Turn{
		planNoteTurn(0, "upsert", "trip", `[{"id":"a","label":"pack","status":"in_progress"}]`),
		planNoteTurn(1, "upsert", "trip", `[{"id":"a","label":"pack","status":"done"}]`),
	})
	require.Len(t, items, 1)
	require.Equal(t, "plan", items[0].Kind)
	assert.Contains(t, string(items[0].Plan.SnapshotJSON), `"status":"done"`)
	assert.NotContains(t, string(items[0].Plan.SnapshotJSON), `"in_progress"`)
}

// TestHasOpeningTurn pins the exact predicate a live mirror uses to decide
// whether the runner has placed the session's opening turn yet — the window in
// which the mirror must render the opening message from the AgentSession's own
// spec instead. The distinction that matters is INDEX 0 AND ROLE user: a
// transcript can have later turns while turn 0 is still absent (a cold-start
// review that placed none), and "the transcript is empty" would miss that.
func TestHasOpeningTurn(t *testing.T) {
	cases := []struct {
		name  string
		turns []memory.Turn
		want  bool
	}{
		{name: "no turns at all: not placed", turns: nil, want: false},
		{name: "turn 0 user: placed", turns: []memory.Turn{textTurn(0, "user", "hello")}, want: true},
		{
			name:  "later turns but no turn 0: not placed",
			turns: []memory.Turn{respondTurn(1, "hi"), textTurn(2, "user", "still here?")},
			want:  false,
		},
		{
			name:  "an inbox turn at index 0 is not the runner's placement",
			turns: []memory.Turn{textTurn(0, "inbox", "hello")},
			want:  false,
		},
		{
			name:  "turn 0 found out of order",
			turns: []memory.Turn{respondTurn(1, "hi"), textTurn(0, "user", "hello")},
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, turn.HasOpeningTurn(tc.turns))
		})
	}
}
