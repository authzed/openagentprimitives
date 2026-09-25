package livemirror

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/toolenvelope"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// TestReadHistory_RendersStreamedToolkitResultAndKeepsOrdinaryOnesHidden is the
// reported bug at the reload boundary, in both directions at once.
//
// A streaming sub-agent toolkit streams its work to the browser while it runs
// and finishes with a report composed FOR A HUMAN — a status line over the
// sub-agent's own prose. Reload used to drop it: nothing in the transcript said
// that result differed from a memory query's rows, so the shared mapping hid
// both. The user watched a sub-agent do the work and then found no trace of it,
// which is exactly the "live == reload" invariant this file already holds
// itself to for a deleted plan card.
//
// The fixture puts BOTH kinds of result in ONE relay turn, because that is what
// a real tool batch looks like and because a per-turn fix would pass a test that
// separated them.
func TestReadHistory_RendersStreamedToolkitResultAndKeepsOrdinaryOnesHidden(t *testing.T) {
	const name = "demo-agent-stream"
	const report = "status: success (7.3s, $0.09)\nI reviewed the README and found no typos."
	const ordinary = "id,tags\n1,alpha\n2,beta"
	at := time.Now().UTC().Truncate(time.Second)

	entries := []memory.Entry{
		historyTestEntry(t, name, 0, "user", "have the sub-agent review the README", at),
		// The assistant turn that made the calls is tool-only: no visible text,
		// dropped as before. It is present so the fixture is the real sequence.
		historyBlockEntry(t, name, 1, "assistant", at.Add(time.Second),
			memory.ContentBlock{Type: "tool_use", ToolUse: &memory.ToolUseBlock{
				ID: "tu_1", Name: "codelike_subagent", Input: []byte(`{}`),
			}},
		),
		historyBlockEntry(t, name, 2, "user", at.Add(2*time.Second),
			historyResultBlock("tu_1", report, true),
			historyResultBlock("tu_2", ordinary, false),
		),
		historyTestEntry(t, name, 3, "assistant", "all done", at.Add(3*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	hist, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	got := hist.Timeline
	require.Len(t, got, 3, "the user message, the streamed report, and the agent reply")

	assert.Equal(t, "user", got[0].Role)

	assert.Equal(t, "message", got[1].Kind)
	assert.Equal(t, "agent", got[1].Role, "the report is the agent's work, not the human's, even though the relay turn borrows the 'user' role")
	assert.Equal(t, report, got[1].Text)
	assert.Equal(t, at.Add(2*time.Second), got[1].CreatedAt, "it sits where the tool answered, keeping live order")
	assert.NotContains(t, got[1].Text, toolenvelope.Tag, "the model-facing envelope must never reach a chat bubble")
	assert.NotContains(t, got[1].Text, ordinary, "an ordinary tool result must stay hidden even when batched with a shown one")

	assert.Equal(t, "agent", got[2].Role)
	assert.Equal(t, "all done", got[2].Text)
}

// historyBlockEntry is historyTestEntry for a turn whose content is arbitrary
// blocks rather than a single text block.
func historyBlockEntry(t *testing.T, name string, idx int, role string, at time.Time, blocks ...memory.ContentBlock) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]any{"content": blocks})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: testScope + "/" + name},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   content,
	}
}

// historyResultBlock builds a stored tool_result block, enveloped the way the
// runner persists it. renderedLive says the live surfaces already showed it.
func historyResultBlock(id, content string, renderedLive bool) memory.ContentBlock {
	return memory.ContentBlock{Type: "tool_result", ToolResult: &memory.ToolResultBlock{
		ToolUseID:    id,
		Content:      toolenvelope.Wrap(content, "cafebabe0badf00d"),
		RenderedLive: renderedLive,
	}}
}
