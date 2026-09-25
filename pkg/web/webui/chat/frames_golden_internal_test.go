package chat

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/browser"
	"github.com/authzed/openagentprimitives/pkg/channels/sessionnotice"
)

// goldenFramesPath is the ONE artifact both halves of the session-socket →
// UI-view seam read: toFrame's real output for a scripted turn, and the TS
// reducer (sessionSignals.ts) folding those same bytes into the signals the
// agent-defined view's fallbacks derive from. A literal in each language's
// own test is not a pin — see agentui's live golden for the argument.
const goldenFramesPath = "ui/testdata/frames.golden.json"

func goldenFrameSequence() []any {
	ref := browser.SessionRef{Namespace: "workshop", Name: "sess-live"}
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	return []any{
		browser.MsgTurnActivity{Session: ref, Active: true},
		browser.MsgStreamDelta{Session: ref, Payload: channelevents.AssistantStreamDeltaPayload{EventType: "tool_use_start", ToolName: "update_view", ToolID: "tu-1", BlockIdx: 1}},
		browser.MsgOperationActivity{Session: ref, Payload: channelevents.OperationActivityPayload{CompactLine: "Connecting GitHub"}},
		browser.MsgPlanUpdate{Session: ref, Payload: channelevents.PlanUpdatePayload{PlanName: "build", UpdatedAt: at, Items: []channelevents.PlanItemRef{
			{ID: "intake", Label: "Understand the brief", Status: "done"},
			{ID: "tools", Label: "Connect tools", Status: "in_progress"},
			{ID: "perms", Label: "Set permissions", Status: "pending"},
		}}},
		browser.MsgStreamDelta{Session: ref, Payload: channelevents.AssistantStreamDeltaPayload{EventType: "tool_use_stop", ToolID: "tu-1", BlockIdx: 1}},
		browser.MsgUserMessage{Session: ref, Text: "Which repository should it watch?"},
		browser.MsgTurnActivity{Session: ref, Active: false, Cause: channelevents.PauseCauseReply},
		browser.MsgUserEcho{Session: ref, Text: "org/repo", Via: "urn:demo"},
		browser.MsgTurnActivity{Session: ref, Active: true},
		browser.MsgOperationActivity{Session: ref, Payload: channelevents.OperationActivityPayload{Cleared: true}},
		// The idle exit, which is what a channel-attached session actually parks
		// on once the agent's work is done: the runner publishes
		// PauseCauseComplete on no turn_activity, so pinning it here would pin a
		// frame the wire never carries.
		browser.MsgTurnActivity{Session: ref, Active: false, Cause: channelevents.PauseCauseIdle},
		// A plan-amendment prompt the agent is blocked on — the interaction
		// request/applied pair the agent-defined UI view's "Needs your
		// decision" region folds (Task 2's pendingInteractions). Pinning both
		// halves here is what keeps sessionSignals.ts's fold and the wire shape
		// from drifting apart.
		browser.MsgInteractionRequest{Session: ref, Payload: channelevents.InteractionRequestPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: "workshop", Name: "sess-live"},
			Category:        "plan_amendment",
			RequestRef:      "req-1",
			Lead:            "The agent is asking to add workshop_apply to its approved plan.",
			Actions: []channelevents.InteractionAction{
				{ID: "approve", Label: "Approve", Kind: channelevents.ActionKindDecision},
			},
		}},
		browser.MsgInteractionApplied{Session: ref, Payload: channelevents.InteractionAppliedPayload{
			AgentSessionRef: channelevents.SessionRef{Namespace: "workshop", Name: "sess-live"},
			Category:        "plan_amendment",
			RequestRef:      "req-1",
			Outcome:         channelevents.OutcomeApproved,
		}},
		// The session is torn down. It is the registry that emits this, not a
		// sender, and it is the frame the reply fallback's `ended` gate reads:
		// an ended session has nobody left to answer, so pinning its wire shape
		// here is what keeps that gate honest.
		sessionEndedMsg{Session: ref, Reason: "succeeded"},
		// The startup line's frame, pinned LAST so every checkpoint the TS
		// reducer test already indexes keeps its position. Chronologically it
		// belongs before the first turn_activity — the health watcher emits it
		// on every pre-start tick — but this golden pins shapes, and the
		// reducer folds it independently of anything before it.
		sessionStartupMsg{Session: ref, Text: sessionnotice.GenericStartupLead, StillTrying: true},
	}
}

func TestFramesGoldenMatchesToFrame(t *testing.T) {
	var frames []json.RawMessage
	for _, m := range goldenFrameSequence() {
		f, ok := toFrame(m)
		require.True(t, ok, "%T must map to a wire frame", m)
		raw, err := json.Marshal(f)
		require.NoError(t, err)
		frames = append(frames, raw)
	}
	got, err := json.MarshalIndent(map[string]any{"frames": frames}, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	if os.Getenv("REGEN_GOLDEN") == "1" {
		require.NoError(t, os.WriteFile(goldenFramesPath, got, 0o644))
	}
	want, err := os.ReadFile(goldenFramesPath)
	require.NoError(t, err, "the golden the TS reducer test reads must exist (REGEN_GOLDEN=1 writes it)")
	assert.JSONEq(t, string(want), string(got), "the session-socket frame shapes changed: update sink.go, %s, and sessionSignals.ts together", goldenFramesPath)
}
