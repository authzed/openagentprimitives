package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// Role mapping, respond_to_user extraction, tool-only dropping, and inbox
// de-duplication are unit-tested in pkg/memory/kinds/turn (TestVisibleMessages).
// The tests here cover chat's use of it end-to-end via readTranscript.

func TestSessionTitle(t *testing.T) {
	at := time.Date(2026, 7, 4, 9, 30, 0, 0, time.UTC)
	assert.Equal(t, "Hello there", sessionTitle("Hello there", "demo-agent", at))
	assert.Equal(t, "Line one", sessionTitle("  Line one\nLine two", "demo-agent", at), "only the first line is used")
	assert.Equal(t, "demo-agent · 09:30", sessionTitle("   ", "demo-agent", at), "empty text falls back to class · time")

	long := ""
	for i := 0; i < 100; i++ {
		long += "x"
	}
	got := sessionTitle(long, "demo-agent", at)
	assert.LessOrEqual(t, len([]rune(got)), titleMaxRunes+1, "long titles are truncated (plus the ellipsis rune)")
	assert.Contains(t, got, "…")
}

func TestReadTranscript_NoMemoryAccessReturnsSentinel(t *testing.T) {
	_, err := readTranscript(context.Background(), &fakeDeps{}, newChatSessionNamespace, "s")
	assert.True(t, errors.Is(err, ErrNoMemoryAccess))
}

func TestReadTranscript_MapsRolesSkipsEmptyAndPreservesOrder(t *testing.T) {
	const name = "demo-agent-x"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		turnTestEntry(t, name, 0, "user", "please help", at),
		turnTestEntry(t, name, 1, "assistant", "sure thing", at.Add(time.Second)),
		turnTestEntry(t, name, 2, "assistant", "", at.Add(2*time.Second)),           // tool-only turn: dropped
		turnTestEntry(t, name, 3, "system_note", "internal", at.Add(3*time.Second)), // non-conversational: dropped
		turnTestEntry(t, name, 4, "inbox", "a follow-up", at.Add(4*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// turn.ReadAll issues GET /memory/turn/{ns}/{name}.
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := &fakeDeps{operatorURL: srv.URL, memoryToken: "webd-token"}
	got, err := readTranscript(context.Background(), d, newChatSessionNamespace, name)
	require.NoError(t, err)
	require.Len(t, got, 3, "the empty assistant turn and the system_note are dropped")
	assert.Equal(t, timelineEntry{Kind: "message", Role: "user", Text: "please help", CreatedAt: at}, got[0])
	assert.Equal(t, "message", got[1].Kind)
	assert.Equal(t, "agent", got[1].Role)
	assert.Equal(t, "sure thing", got[1].Text)
	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "user", got[2].Role, "an inbox turn renders as a user message")
	assert.Equal(t, "a follow-up", got[2].Text)
}

// TestReadTranscript_DeduplicatesDrainedInbox reproduces the reported bug at
// the readTranscript boundary: a mid-session message survives in memory as an
// "inbox" turn, an "inbox_done" marker, AND the "user" turn the runner promoted
// it into. It must render exactly once.
func TestReadTranscript_DeduplicatesDrainedInbox(t *testing.T) {
	const name = "demo-agent-dedup"
	at := time.Now().UTC().Truncate(time.Second)
	entries := []memory.Entry{
		turnTestEntry(t, name, 0, "user", "what time of the day is it?", at),
		turnTestEntry(t, name, 1, "assistant", "the tides, not the hour!", at.Add(time.Second)),
		turnTestEntry(t, name, 2, "inbox", "how many milliseconds to the full moon?", at.Add(2*time.Second)),
		turnTestEntry(t, name, 2, "inbox_done", "inbox entry consumed", at.Add(2*time.Second)),
		turnTestEntry(t, name, 3, "user", "how many milliseconds to the full moon?", at.Add(3*time.Second)),
		turnTestEntry(t, name, 4, "assistant", "no charts fer that, matey!", at.Add(4*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := &fakeDeps{operatorURL: srv.URL, memoryToken: "webd-token"}
	got, err := readTranscript(context.Background(), d, newChatSessionNamespace, name)
	require.NoError(t, err)
	require.Len(t, got, 4, "the drained inbox turn must not double the message")
	assert.Equal(t, "message", got[2].Kind)
	assert.Equal(t, "how many milliseconds to the full moon?", got[2].Text)
	assert.Equal(t, "user", got[2].Role)
	assert.Equal(t, "message", got[3].Kind)
	assert.Equal(t, "no charts fer that, matey!", got[3].Text, "the agent reply follows immediately, not a second copy")
}

// TestReadTranscript_ReconstructsPlanCard covers the resumed-conversation
// path: a plan card published mid-session via update_plan is recorded as a
// "plans" system_note turn (see plans.Store.persistNote), and a reload must
// reconstruct the same card — positioned where it first appeared, between the
// surrounding messages — not just the user/agent text.
func TestReadTranscript_ReconstructsPlanCard(t *testing.T) {
	const name = "demo-agent-plan"
	at := time.Now().UTC().Truncate(time.Second)
	const planNote = `{"kind":"plans","v":1,"data":{"op":"upsert","plan":{"name":"trip","items":[{"id":"a","label":"pack","status":"done"}],"updated_at":"2026-01-01T00:00:00Z"}}}`
	entries := []memory.Entry{
		turnTestEntry(t, name, 0, "user", "let's plan a trip", at),
		turnTestEntry(t, name, 1, "system_note", planNote, at.Add(time.Second)),
		turnTestEntry(t, name, 2, "assistant", "here's your plan", at.Add(2*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := &fakeDeps{operatorURL: srv.URL, memoryToken: "webd-token"}
	got, err := readTranscript(context.Background(), d, newChatSessionNamespace, name)
	require.NoError(t, err)
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

// TestReadTranscript_DeletedPlanRendersCancelledStub pins the delete edge: when
// a plan is deleted (update_plan with empty items), the live path publishes an
// empty-items "cancelled stub" card, so a reload must reproduce that stub — an
// empty-items card at the plan's position — NOT drop it, keeping live == reload.
func TestReadTranscript_DeletedPlanRendersCancelledStub(t *testing.T) {
	const name = "demo-agent-plandel"
	at := time.Now().UTC().Truncate(time.Second)
	const upsert = `{"kind":"plans","v":1,"data":{"op":"upsert","plan":{"name":"trip","items":[{"id":"a","label":"pack","status":"done"}],"updated_at":"2026-01-01T00:00:00Z"}}}`
	const del = `{"kind":"plans","v":1,"data":{"op":"delete","name":"trip"}}`
	entries := []memory.Entry{
		turnTestEntry(t, name, 0, "user", "plan then scrap it", at),
		turnTestEntry(t, name, 1, "system_note", upsert, at.Add(time.Second)),
		turnTestEntry(t, name, 2, "system_note", del, at.Add(2*time.Second)),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: entries}))
	}))
	defer srv.Close()

	d := &fakeDeps{operatorURL: srv.URL, memoryToken: "webd-token"}
	got, err := readTranscript(context.Background(), d, newChatSessionNamespace, name)
	require.NoError(t, err)
	require.Len(t, got, 2, "the user message and the cancelled-stub card (not dropped)")
	assert.Equal(t, "plan", got[1].Kind)
	require.NotNil(t, got[1].Plan)
	assert.Equal(t, "trip", got[1].Plan.PlanName)
	assert.Empty(t, got[1].Plan.Items, "a deleted plan renders as an empty-items 'cancelled stub', matching live")
}

// turnTestEntry builds a memory Entry in the on-wire shape turn.EntryToTurn
// decodes: the (index, role) live in the ID and the text lives in a
// {"content":[{"type":"text","text":...}]} content blob.
func turnTestEntry(t *testing.T, name string, idx int, role, text string, at time.Time) memory.Entry {
	t.Helper()
	content, err := json.Marshal(map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	require.NoError(t, err)
	return memory.Entry{
		Scope:     memory.Scope{Kind: "session", ID: newChatSessionNamespace + "/" + name},
		Kind:      turn.KindName,
		ID:        turn.EntryID(idx, role),
		CreatedAt: at,
		Content:   content,
	}
}

// TestReadTranscript_OpeningPromptBeforeTurnZeroIsDurable covers the reported
// bug: the dashboard's start route puts the viewer's opening message on
// AgentSession.spec.prompt.inline and navigates immediately, but the runner
// does not place its durable turn 0 until its pod is up seconds later. The
// mount-time transcript read lands in that window, and nothing live carries
// turn 0 (user_echo is published only for view-originated messages), so the
// viewer's own first message was absent until a manual reload.
//
// The predicate is the PRESENCE OF TURN 0, not an empty transcript: a session
// whose cold-start review placed no turn 0 still has other turns, and its
// opening message must render exactly once there too.
func TestReadTranscript_OpeningPromptBeforeTurnZeroIsDurable(t *testing.T) {
	const prompt = "chart the tides for the morning watch"
	at := time.Now().UTC().Truncate(time.Second)
	createdAt := at.Add(-time.Minute)

	cases := []struct {
		name    string
		entries func(sessName string) []memory.Entry
		// withoutSession omits the AgentSession from the fake client, standing
		// in for a conversation whose object is gone but whose transcript is
		// still replayable.
		withoutSession bool
		wantTexts      []string
		wantRoles      []string
	}{
		{
			name:      "no turns yet: the opening prompt renders as the viewer's first message",
			entries:   func(string) []memory.Entry { return nil },
			wantTexts: []string{prompt},
			wantRoles: []string{"user"},
		},
		{
			name: "turn 0 is durable: the opening prompt is not doubled",
			entries: func(n string) []memory.Entry {
				return []memory.Entry{
					turnTestEntry(t, n, 0, "user", prompt, at),
					turnTestEntry(t, n, 1, "assistant", "high water at six bells", at.Add(time.Second)),
				}
			},
			wantTexts: []string{prompt, "high water at six bells"},
			wantRoles: []string{"user", "agent"},
		},
		{
			name: "turns exist but none is turn 0: the opening prompt still renders, first",
			entries: func(n string) []memory.Entry {
				return []memory.Entry{
					turnTestEntry(t, n, 1, "assistant", "that request was refused", at.Add(time.Second)),
				}
			},
			wantTexts: []string{prompt, "that request was refused"},
			wantRoles: []string{"user", "agent"},
		},
		{
			name:           "the AgentSession is gone: the durable transcript replays unchanged",
			entries:        func(string) []memory.Entry { return nil },
			withoutSession: true,
			wantTexts:      nil,
			wantRoles:      nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name := "demo-agent-opening"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(memory.QueryResult{Entries: tc.entries(name)}))
			}))
			defer srv.Close()

			var objs []client.Object
			if !tc.withoutSession {
				objs = append(objs, &spiceboxv1alpha1.AgentSession{
					ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: newChatSessionNamespace,
						CreationTimestamp: metav1.NewTime(createdAt),
					},
					Spec: spiceboxv1alpha1.AgentSessionSpec{
						Class:  "demo-agent",
						Prompt: spiceboxv1alpha1.PromptSource{Inline: prompt},
					},
				})
			}
			d := &fakeDeps{operatorURL: srv.URL, memoryToken: "webd-token", k8s: newFakeK8sClient(t, objs...)}

			got, err := readTranscript(context.Background(), d, newChatSessionNamespace, name)
			require.NoError(t, err)
			require.NotNil(t, got, `an empty conversation is [] on the wire, never "timeline": null`)
			require.Len(t, got, len(tc.wantTexts))
			for i := range tc.wantTexts {
				assert.Equal(t, "message", got[i].Kind)
				assert.Equal(t, tc.wantRoles[i], got[i].Role)
				assert.Equal(t, tc.wantTexts[i], got[i].Text)
			}
			if len(tc.wantTexts) > 0 && !tc.withoutSession && tc.wantRoles[0] == "user" {
				assert.False(t, got[0].CreatedAt.IsZero(), "the opening message carries a timestamp so it sorts first")
			}
		})
	}
}
