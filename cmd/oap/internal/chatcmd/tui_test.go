package chatcmd

import (
	"fmt"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	local "github.com/authzed/openagentprimitives/pkg/channels/channelkinds/local"
)

// sized returns the model after a WindowSizeMsg so layout is computed.
func sized(m chatModel) chatModel {
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return updated.(chatModel)
}

func TestChatModel_UserAndAgentMessages_AppearInTimeline(t *testing.T) {
	m := sized(newChatModel("demo-agent", "demo-agent-a1b2c3", true))
	m = applyMsg(t, m, local.MsgUserMessage{Text: "the README is written"})
	m = applyMsg(t, m, local.MsgUserMessage{Text: "PR opened"})
	view := m.View()
	assert.Contains(t, view, "the README is written")
	assert.Contains(t, view, "PR opened")
	assert.Contains(t, view, "agent")
}

func TestChatModel_LocalEcho_RendersTypedInput(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, msgLocalEcho{Text: "do the thing"})
	assert.Contains(t, m.View(), "do the thing")
	assert.Contains(t, m.View(), "you")
}

// TestChatModel_AgentUIOffer_RendersTheDashboardLine pins the one production
// wiring that turns a published offer into something the user can see. The
// sender and its event are covered in pkg/channels/channelkinds/local, but deleting
// this model's render arm leaves every one of those tests green while the
// terminal shows nothing — the shape where thoroughly-tested logic reaches a
// surface through an untested line.
//
// It also pins the line's own wording. A separate event exists precisely so an
// agent-UI handoff is distinguishable from an artifact's live view in the
// timeline; rendering it with the shared "View in browser" phrasing would give
// up that distinction without failing anything.
func TestChatModel_AgentUIOffer_RendersTheDashboardLine(t *testing.T) {
	// The URL is kept short on purpose: the timeline wraps at the viewport
	// width, and a longer one is split across lines by the box border, so a
	// whole-URL assertion would fail on the renderer doing its job rather than
	// on the arm being absent.
	const wantURL = "https://w.example/sessions?session=demo-ns%2Fs1"
	m := sized(newChatModel("demo-agent", "demo-agent-a1b2c3", true))
	m = applyMsg(t, m, local.MsgAgentUIOffer{URL: wantURL})
	view := m.View()
	assert.Contains(t, view, wantURL,
		"the offer is worthless without the address the user has to follow")
	assert.Contains(t, view, "dashboard",
		"the line must name what it opens, not read as a generic browser link")
}

// applyMsg pushes one tea.Msg through Update and returns the new model.
func applyMsg(t *testing.T, m chatModel, msg tea.Msg) chatModel {
	t.Helper()
	updated, _ := m.Update(msg)
	return updated.(chatModel)
}

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

var _ = channelevents.KindUserMessage // keep the import while stubbing

func TestChatModel_ToolSession_RendersAndCollapses(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgToolSessionEvent{Payload: channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-1", EventType: "tool_use_start",
		OuterTool: "claude", Reason: "write the README", ToolName: "Write",
	}})
	m = applyMsg(t, m, local.MsgToolSessionEvent{Payload: channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-1", EventType: "result", OuterTool: "claude", OK: true,
		DurationMs: 97000, CostUSD: 0.12,
	}})
	expanded := m.View()
	assert.Contains(t, expanded, "claude · write the README")
	assert.Contains(t, expanded, "✓ done")
	assert.Contains(t, expanded, "▼", "expanded tool block shows ▼")

	// space collapses the newest tool block (input box empty).
	m = applyMsg(t, m, keyMsg(" "))
	assert.Contains(t, m.View(), "▶", "collapsed tool block shows ▶")
}

func TestChatModel_ToolUseStart_ShowsToolCommand(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	// A tool_use_start carries the streaming agent's sub-tool name AND
	// its input summary (the command/args). Both must render — showing
	// only the bare tool name hides what the agent is actually doing.
	m = applyMsg(t, m, local.MsgToolSessionEvent{Payload: channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-1", EventType: "tool_use_start",
		OuterTool: "claude", ToolName: "Bash", Summary: "$ go test ./...",
	}})
	view := m.View()
	assert.Contains(t, view, "Bash", "sub-tool name shown")
	assert.Contains(t, view, "$ go test ./...", "the tool's command is shown, not just the name")
}

func TestChatModel_ToolDelta_Tail_IsLineBounded(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	for i := 0; i < toolTailMax+4; i++ {
		m = applyMsg(t, m, local.MsgToolSessionDelta{Payload: channelevents.ToolSessionDeltaPayload{
			ToolCallRef: "tc-1", Stream: "stdout",
			Data: []byte(fmt.Sprintf("line-%d\n", i)),
		}})
	}
	view := m.View()
	assert.NotContains(t, view, "line-0", "oldest tail line dropped")
	assert.Contains(t, view, fmt.Sprintf("line-%d", toolTailMax+3), "newest tail line kept")
}

func TestChatModel_PlanOverlay_OpensAndShowsItems(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgPlanUpdate{Payload: channelevents.PlanUpdatePayload{
		PlanName: "main",
		Items: []channelevents.PlanItemRef{
			{ID: "i1", Label: "inspect repo", Status: "done"},
			{ID: "i2", Label: "write README", Status: "in_progress"},
			{ID: "i3", Label: "open PR", Status: "pending"},
		},
	}})
	// status line shows the compact indicator.
	assert.Contains(t, m.View(), "plan 1/3")
	// p opens the overlay (input box empty).
	m = applyMsg(t, m, keyMsg("p"))
	overlay := m.View()
	assert.Contains(t, overlay, "Plan — main")
	assert.Contains(t, overlay, "inspect repo")
	assert.Contains(t, overlay, "write README")
	// p closes it.
	m = applyMsg(t, m, keyMsg("p"))
	assert.NotContains(t, m.View(), "Plan — main")
}

func TestChatModel_SessionFailed_DisablesInput(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, msgSessionPhase{Phase: "Failed", FailureReason: "BudgetExhausted"})
	view := m.View()
	assert.Contains(t, view, "session failed — BudgetExhausted")
	assert.Contains(t, view, "input disabled")
	assert.Contains(t, view, "press q to quit", "footer/timeline tells the user how to exit")
	// Enter is a no-op once ended.
	before := len(m.items)
	m.input.SetValue("more")
	m = applyMsg(t, m, keyMsg("enter"))
	assert.Equal(t, before, len(m.items), "no new item appended after the session ended")
}

// TestChatModel_SessionTerminal_InputGenuinelyDisabled audits that once
// the session is terminal NO key can reach the input box or invoke
// submit — typed runes are swallowed, Enter does not submit, and q/esc
// quit without needing an empty-input guard. This is the airtight
// "no message reaches the pipeline after the session ended" check.
func TestChatModel_SessionTerminal_InputGenuinelyDisabled(t *testing.T) {
	terminalCases := []struct {
		name string
		msg  msgSessionPhase
	}{
		{name: "Failed terminal", msg: msgSessionPhase{Phase: "Failed", FailureReason: "BudgetExhausted"}},
		{name: "Succeeded terminal", msg: msgSessionPhase{Phase: "Succeeded"}},
	}
	for _, tc := range terminalCases {
		t.Run(tc.name+": typed runes are swallowed, submit never fires", func(t *testing.T) {
			submitted := 0
			m := sized(newChatModel("demo-agent", "s1", true))
			m.submit = func(string) { submitted++ }
			m = applyMsg(t, m, tc.msg)
			require.True(t, m.ended, "model must be in the ended state")

			// Typing a rune must not reach the input box.
			m = applyMsg(t, m, keyMsg("x"))
			assert.Empty(t, m.input.Value(), "input box is dead — typed runes are swallowed")

			// Even if text was somehow present, Enter must not submit.
			m.input.SetValue("smuggled message")
			before := len(m.items)
			m = applyMsg(t, m, keyMsg("enter"))
			assert.Equal(t, before, len(m.items), "Enter appends nothing after the session ended")
			assert.Equal(t, 0, submitted, "submit must NEVER fire once the session is terminal")
		})

		t.Run(tc.name+": q quits regardless of stale input text", func(t *testing.T) {
			m := sized(newChatModel("demo-agent", "s1", true))
			m = applyMsg(t, m, tc.msg)
			// q must quit even though the input box has leftover text —
			// once ended there is no "q is text" ambiguity.
			m.input.SetValue("leftover")
			_, cmd := m.Update(keyMsg("q"))
			require.NotNil(t, cmd, "q must return a command once the session ended")
			assert.Equal(t, tea.Quit(), cmd(), "q quits the program")
		})
	}
}

// TestChatModel_SessionSucceeded_ShowsOutcome verifies a Succeeded
// terminal phase renders a clear "session ended" outcome and disables
// input — not a bare phase word.
func TestChatModel_SessionSucceeded_ShowsOutcome(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, msgSessionPhase{Phase: "Succeeded"})
	view := m.View()
	assert.True(t, m.ended, "Succeeded is terminal")
	assert.Contains(t, view, "session ended")
	assert.Contains(t, view, "input disabled")
	assert.Contains(t, view, "press q to quit")
}

// TestChatModel_InboundResult_NoActiveSession_EndsInteraction verifies
// that an msgInboundResult with Ended=true (the pipeline returned
// OutcomeNoActiveSession — it refused to spawn a phantom session) is
// shown visibly AND ends the interaction, exactly like a terminal phase.
func TestChatModel_InboundResult_NoActiveSession_EndsInteraction(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, msgInboundResult{Routed: false, Ended: true})
	view := m.View()
	assert.True(t, m.ended, "an Ended inbound result ends the interaction")
	assert.Contains(t, view, "the session has ended", "the user is told the session ended")
	assert.Contains(t, view, "input disabled")
	assert.Contains(t, view, "press q to quit")
}

// TestChatModel_InboundResult_NeverSilent verifies every non-routed
// inbound result is rendered VISIBLY — no silent drops (AGENTS.md).
func TestChatModel_InboundResult_NeverSilent(t *testing.T) {
	cases := []struct {
		name string
		msg  msgInboundResult
		want string
	}{
		{
			name: "deny notice: shows the notice text",
			msg: msgInboundResult{Routed: false, Notice: &channelevents.NoticeWire{
				Tone: "warning", Lead: "not allowed",
			}},
			want: "not allowed",
		},
		{
			name: "delivery error: shows the error prominently",
			msg:  msgInboundResult{Routed: false, Err: "nats timeout"},
			want: "message not delivered: nats timeout",
		},
		{
			name: "no detail: still surfaces a delivery failure",
			msg:  msgInboundResult{Routed: false},
			want: "message not delivered",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(newChatModel("demo-agent", "s1", true))
			before := len(m.items)
			m = applyMsg(t, m, tc.msg)
			assert.Greater(t, len(m.items), before, "a non-routed result must append a visible note")
			assert.Contains(t, m.View(), tc.want)
		})
	}
}

func TestChatModel_SessionFailed_RendersReasonAndMessage(t *testing.T) {
	cases := []struct {
		name string
		msg  msgSessionPhase
		want string
	}{
		{
			name: "reason+message: shows 'Reason: message'",
			msg:  msgSessionPhase{Phase: "Failed", FailureReason: "RunnerCrashed", FailureMessage: "runner restarted 5 times"},
			want: "session failed — RunnerCrashed: runner restarted 5 times",
		},
		{
			name: "reason only: shows the bare reason",
			msg:  msgSessionPhase{Phase: "Failed", FailureReason: "BudgetExhausted"},
			want: "session failed — BudgetExhausted",
		},
		{
			name: "message only: shows the bare message",
			msg:  msgSessionPhase{Phase: "Failed", FailureMessage: "pod evicted"},
			want: "session failed — pod evicted",
		},
		{
			name: "neither: shows 'unknown'",
			msg:  msgSessionPhase{Phase: "Failed"},
			want: "session failed — unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sized(newChatModel("demo-agent", "s1", true))
			m = applyMsg(t, m, tc.msg)
			assert.Contains(t, m.View(), tc.want)
		})
	}
}

func TestChatModel_SessionStarting_ShowsStartingState(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	// Before the first message: the status line is plain "idle".
	assert.Contains(t, m.View(), "idle")
	assert.NotContains(t, m.View(), "starting the agent")

	// First message submitted → the starter emits msgSessionStarting.
	m = applyMsg(t, m, msgSessionStarting{})
	starting := m.View()
	assert.True(t, m.starting, "model is in the starting state")
	assert.Contains(t, starting, "starting the agent…", "status line / timeline shows it is waiting on the session")
	assert.NotContains(t, starting, "idle", "starting state is distinct from idle")

	// First agent output clears the starting state.
	m = applyMsg(t, m, local.MsgUserMessage{Text: "on it"})
	assert.False(t, m.starting, "agent output clears the starting state")
	assert.Contains(t, m.View(), "on it")
}

func TestChatModel_Header_SessionID_HiddenUntilFirstMessage(t *testing.T) {
	const sessionID = "demo-agent-a1b2c3"
	m := sized(newChatModel("demo-agent", sessionID, true))

	// Before the first message the AgentSession does not exist yet, so
	// the header must NOT show a (misleading) live-looking session id —
	// just the agent-class and a hint to start typing.
	before := m.View()
	assert.Contains(t, before, "oap agent chat — demo-agent", "header shows the agent-class")
	assert.NotContains(t, before, sessionID, "session id is hidden before the session exists")
	assert.Contains(t, before, "type a message to start", "header hints the user should type to start")

	// The first message triggers session creation (msgSessionStarting);
	// from here on the id is real and the header shows it.
	m = applyMsg(t, m, msgSessionStarting{})
	after := m.View()
	assert.Contains(t, after, sessionID, "session id is shown once session creation has begun")
	assert.NotContains(t, after, "type a message to start", "start hint is gone once the session is live")
}

func TestChatModel_SessionStarting_ClearedByToolEvent(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, msgSessionStarting{})
	assert.True(t, m.starting)
	// A tool-session event is also "first output" — it clears starting.
	m = applyMsg(t, m, local.MsgToolSessionEvent{Payload: channelevents.ToolSessionEventPayload{
		ToolCallRef: "tc-1", EventType: "tool_use_start", OuterTool: "claude", ToolName: "Write",
	}})
	assert.False(t, m.starting, "a tool event clears the starting state")
}

func TestChatModel_SendError_RendersWarning(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, local.MsgSendError{
		Kind: channelevents.KindUserMessage, Err: "decode failed",
	})
	assert.Contains(t, m.View(), "render error")
	assert.Contains(t, m.View(), "decode failed")
}

func TestFmtSessionElapsed(t *testing.T) {
	cases := []struct {
		name string
		d    time.Duration
		want string
	}{
		{name: "zero → 0s", d: 0, want: "0s"},
		{name: "negative → 0s", d: -5 * time.Second, want: "0s"},
		{name: "45s", d: 45 * time.Second, want: "45s"},
		{name: "1m23s", d: 83 * time.Second, want: "1m23s"},
		{name: "1h02m", d: time.Hour + 2*time.Minute + 30*time.Second, want: "1h02m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fmtSessionElapsed(tc.d))
		})
	}
}

// TestChatModel_SessionElapsedTimer verifies that the header shows an
// elapsed timer once the session is live, and does NOT show one before
// the first message (when there is no session yet).
func TestChatModel_SessionElapsedTimer(t *testing.T) {
	t.Run("no timer before session starts", func(t *testing.T) {
		m := sized(newChatModel("demo-agent", "demo-agent-a1b2c3", true))
		// Before any message the session is not live — no timer in the header.
		view := m.View()
		// The timer region would contain a digit followed by 's', 'm', or 'h'.
		// "type a message to start" is in the header — we should see no elapsed.
		assert.False(t, m.sessionLive, "session must not be live before first message")
		assert.NotContains(t, view, "0s", "no timer before session starts (no '0s' in header)")
	})

	t.Run("timer present after session starts", func(t *testing.T) {
		m := sized(newChatModel("demo-agent", "demo-agent-a1b2c3", true))
		m = applyMsg(t, m, msgSessionStarting{})
		require.True(t, m.sessionLive, "sessionLive must be set by msgSessionStarting")
		require.False(t, m.sessionStartedAt.IsZero(), "sessionStartedAt must be set")

		// Override sessionStartedAt to a known past time so View() produces
		// a stable, non-zero elapsed we can assert on without wall-clock
		// flakiness.
		m.sessionStartedAt = time.Now().Add(-45 * time.Second)
		view := m.View()
		// The header should now contain the elapsed — at least the 's' suffix
		// and a digit from the timer.
		assert.Contains(t, view, "s", "elapsed timer is present in the header")
		// The session id must also still be present.
		assert.Contains(t, view, "demo-agent-a1b2c3", "session id still shown alongside timer")
	})

	t.Run("timer frozen at end time once session ends", func(t *testing.T) {
		m := sized(newChatModel("demo-agent", "s1", true))
		m = applyMsg(t, m, msgSessionStarting{})
		require.True(t, m.sessionLive)

		// Fake start 2 minutes ago, end 1 minute ago — elapsed should be ~1m.
		now := time.Now()
		m.sessionStartedAt = now.Add(-2 * time.Minute)
		m.sessionEndedAt = now.Add(-1 * time.Minute)
		m.ended = true

		view := m.View()
		// The frozen elapsed is 1 minute = "1m00s".
		assert.Contains(t, view, "1m00s", "elapsed is frozen at end time, not growing further")
	})
}

// streamDelta is one live-LLM stream event as the local kind delivers it.
func streamDelta(eventType, text string) local.MsgStreamDelta {
	return local.MsgStreamDelta{Payload: channelevents.AssistantStreamDeltaPayload{EventType: eventType, Text: text}}
}

// TestChatModel_StreamDeltas_CoalesceTimelineRepaints pins the cost of a
// streamed reply. The runner emits one KindAssistantStreamDelta per
// llm.StreamEventTextDelta and the sink forwards each straight to the
// program with no debounce, so a text_delta arrives roughly per token — while
// refreshTimeline word-wraps and styles the ENTIRE transcript. Repainting per
// token makes the cost of one token linear in the length of the conversation,
// which is why a long session goes sluggish to type in. The repaint must be
// coalesced onto a timer instead.
func TestChatModel_StreamDeltas_CoalesceTimelineRepaints(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	base := m.timelineRenders

	scheduled := 0
	for i := 0; i < 200; i++ {
		updated, cmd := m.Update(streamDelta("text_delta", "tok "))
		m = updated.(chatModel)
		if cmd != nil {
			scheduled++
		}
	}

	assert.Equal(t, 1, scheduled, "a run schedules ONE pending repaint, not one per token")
	assert.Equal(t, base, m.timelineRenders, "a text_delta run defers the transcript re-render")

	m = applyMsg(t, m, msgTimelineFlush{})
	assert.Equal(t, base+1, m.timelineRenders, "the flush repaints once for the whole run")
	assert.Contains(t, m.View(), "tok tok", "every deferred delta reaches the painted transcript")
}

// TestChatModel_StreamStop_PaintsImmediately guards the other side of the
// coalescing: only the per-token path may defer. A run that ends with no
// further messages must not leave its last tokens unpainted behind a timer
// that nothing re-arms.
func TestChatModel_StreamStop_PaintsImmediately(t *testing.T) {
	m := sized(newChatModel("demo-agent", "s1", true))
	m = applyMsg(t, m, streamDelta("text_delta", "the answer"))
	base := m.timelineRenders

	m = applyMsg(t, m, streamDelta("stop", ""))

	assert.Equal(t, base+1, m.timelineRenders, "stream stop repaints synchronously")
	assert.Contains(t, m.View(), "the answer")
}
