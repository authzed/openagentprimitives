package livemirror

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// This test mirrors pkg/web/webui/chat/transcript_test.go's fixture shape so the
// extraction is a verified parity move, not a rewrite: same fake memory HTTP
// endpoint, same turnTestEntry wire shape, same assertions readTranscript's
// tests made — now against the shared, session-agnostic ReadHistory.

const testScope = "history-ns"

func TestReadHistory_NoMemoryAccessReturnsSentinel(t *testing.T) {
	_, err := ReadHistory(context.Background(), "", "", testScope, "s", logr.Discard())
	assert.True(t, errors.Is(err, ErrNoMemoryAccess))

	_, err = ReadHistory(context.Background(), "http://example.invalid", "", testScope, "s", logr.Discard())
	assert.True(t, errors.Is(err, ErrNoMemoryAccess), "empty token alone must also fail closed")
}

func TestReadHistory_MapsRolesSkipsEmptyAndPreservesOrder(t *testing.T) {
	const name = "demo-agent-x"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		historyTestEntry(t, name, 0, "user", "please help", at),
		historyTestEntry(t, name, 1, "assistant", "sure thing", at.Add(time.Second)),
		historyTestEntry(t, name, 2, "assistant", "", at.Add(2*time.Second)),           // tool-only turn: dropped
		historyTestEntry(t, name, 3, "system_note", "internal", at.Add(3*time.Second)), // non-conversational: dropped
		historyTestEntry(t, name, 4, "inbox", "a follow-up", at.Add(4*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// turn.ReadAll issues GET /memory/turn/{ns}/{name}.
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	hist, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	got := hist.Timeline
	require.Len(t, got, 3, "the empty assistant turn and the system_note are dropped")
	assert.Equal(t, TimelineEntry{Kind: "message", Role: "user", Text: "please help", CreatedAt: at}, got[0])
	assert.Equal(t, "message", got[1].Kind)
	assert.Equal(t, "agent", got[1].Role)
	assert.Equal(t, "sure thing", got[1].Text)
	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "user", got[2].Role, "an inbox turn renders as a user message")
	assert.Equal(t, "a follow-up", got[2].Text)
}

// TestReadHistory_DeduplicatesDrainedInbox reproduces the reported bug at the
// ReadHistory boundary: a mid-session message survives in memory as an
// "inbox" turn, an "inbox_done" marker, AND the "user" turn the runner
// promoted it into. It must render exactly once.
func TestReadHistory_DeduplicatesDrainedInbox(t *testing.T) {
	const name = "demo-agent-dedup"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		historyTestEntry(t, name, 0, "user", "what time of the day is it?", at),
		historyTestEntry(t, name, 1, "assistant", "the tides, not the hour!", at.Add(time.Second)),
		historyTestEntry(t, name, 2, "inbox", "how many milliseconds to the full moon?", at.Add(2*time.Second)),
		historyTestEntry(t, name, 2, "inbox_done", "inbox entry consumed", at.Add(2*time.Second)),
		historyTestEntry(t, name, 3, "user", "how many milliseconds to the full moon?", at.Add(3*time.Second)),
		historyTestEntry(t, name, 4, "assistant", "no charts fer that, matey!", at.Add(4*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	hist, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	got := hist.Timeline
	require.Len(t, got, 4, "the drained inbox turn must not double the message")
	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "how many milliseconds to the full moon?", got[2].Text)
	assert.Equal(t, "user", got[2].Role)
	assert.Equal(t, "message", got[3].Kind)
	assert.Equal(t, "no charts fer that, matey!", got[3].Text, "the agent reply follows immediately, not a second copy")
}

// TestReadHistory_ReconstructsPlanCard reproduces the resumed-conversation
// path a plan card published mid-session via update_plan is recorded as a
// "plans" system_note turn, and a reload must reconstruct the same card —
// positioned where it first appeared, between the surrounding messages — not
// just the user/agent text.
func TestReadHistory_ReconstructsPlanCard(t *testing.T) {
	const name = "demo-agent-plan"
	at := time.Now().UTC().Truncate(time.Second)
	const planNote = `{"kind":"plans","v":1,"data":{"op":"upsert","plan":{"name":"trip","items":[{"id":"a","label":"pack","status":"done"}],"updated_at":"2026-01-01T00:00:00Z"}}}`
	entries := []memory.Entry{
		historyTestEntry(t, name, 0, "user", "let's plan a trip", at),
		historyTestEntry(t, name, 1, "system_note", planNote, at.Add(time.Second)),
		historyTestEntry(t, name, 2, "assistant", "here's your plan", at.Add(2*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	hist, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	got := hist.Timeline
	require.Len(t, got, 3, "user message, plan card, agent reply")

	assert.Equal(t, "message", got[0].Kind)
	assert.Equal(t, "user", got[0].Role)
	assert.Equal(t, "let's plan a trip", got[0].Text)

	assert.Equal(t, "plan", got[1].Kind, "the plan card sits between the two messages")
	require.NotNil(t, got[1].Plan)
	assert.Equal(t, "trip", got[1].Plan.PlanName)
	require.Len(t, got[1].Plan.Items, 1)
	assert.Equal(t, channelevents.PlanItemRef{ID: "a", Label: "pack", Status: "done"}, got[1].Plan.Items[0])

	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "agent", got[2].Role)
	assert.Equal(t, "here's your plan", got[2].Text)
}

// TestReadHistory_DeletedPlanRendersCancelledStub pins the delete edge: when a
// plan is deleted (update_plan with empty items), the live path publishes an
// empty-items "cancelled stub" card, so a reload must reproduce that stub — an
// empty-items card at the plan's position — NOT drop it, keeping live ==
// reload.
func TestReadHistory_DeletedPlanRendersCancelledStub(t *testing.T) {
	const name = "demo-agent-plandel"
	at := time.Now().UTC().Truncate(time.Second)
	const upsert = `{"kind":"plans","v":1,"data":{"op":"upsert","plan":{"name":"trip","items":[{"id":"a","label":"pack","status":"done"}],"updated_at":"2026-01-01T00:00:00Z"}}}`
	const del = `{"kind":"plans","v":1,"data":{"op":"delete","name":"trip"}}`
	entries := []memory.Entry{
		historyTestEntry(t, name, 0, "user", "plan then scrap it", at),
		historyTestEntry(t, name, 1, "system_note", upsert, at.Add(time.Second)),
		historyTestEntry(t, name, 2, "system_note", del, at.Add(2*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	hist, err := ReadHistory(context.Background(), srv.URL, "webd-token", testScope, name, logr.Discard())
	require.NoError(t, err)
	got := hist.Timeline
	require.Len(t, got, 2, "the user message and the cancelled-stub card (not dropped)")
	assert.Equal(t, "plan", got[1].Kind)
	require.NotNil(t, got[1].Plan)
	assert.Equal(t, "trip", got[1].Plan.PlanName)
	assert.Empty(t, got[1].Plan.Items, "a deleted plan renders as an empty-items 'cancelled stub', matching live")
}

// historyTestEntry builds a memory Entry in the on-wire shape turn.EntryToTurn
// decodes: the (index, role) live in the ID and the text lives in a
// {"content":[{"type":"text","text":...}]} content blob.
func historyTestEntry(t *testing.T, name string, idx int, role, text string, at time.Time) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: testScope + "/" + name},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   content,
	}
}
