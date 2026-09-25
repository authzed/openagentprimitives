//go:build e2e

package session_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// TestChatResume_NoDuplicateDrainedMessage reproduces the reported chat bug at
// the full E2E layer and pins the fix, across BOTH terminal idioms that idle a
// session between turns.
//
// A mid-session human message travels the REAL runner path: channelsd writes it
// as an "inbox" turn, then the runner's drainInbox promotes it into a "user"
// turn at a new index and writes an "inbox_done" marker at the inbox index —
// leaving all three in append-only memory. When the session is resumed, the
// transcript is rebuilt from exactly these turns. The bug was that both the raw
// "inbox" turn and the promoted "user" turn rendered, so every drained message
// showed twice.
//
// Each subtest drives a two-turn conversation (first message via spec.Prompt,
// second mid-session via the inbox path), ending each turn with a different
// terminal meta tool so the session idles between turns two ways:
//   - agent_work_complete: the round is done, session idle.
//   - await_user_message: the agent yields awaiting the user's next message
//     (IdleTTL is unset in the harness, so it idle-exits and the next message
//     respawns the runner from memory — the close/reopen path the user hit).
//
// It then asserts (1) the raw memory genuinely contains the duplication-prone
// shape (so the dedup isn't vacuous) and (2) turn.VisibleMessages — the single
// source of truth the resumed transcript renders from — collapses it to one
// message per turn.
func TestChatResume_NoDuplicateDrainedMessage(t *testing.T) {
	cases := []struct {
		name       string
		terminator e2e.ReplyPart
	}{
		{
			name:       "ends each turn with agent_work_complete (round done, idle)",
			terminator: e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"}),
		},
		{
			name:       "ends each turn with await_user_message (yields, idle awaiting user)",
			terminator: e2e.ToolUse("await_user_message", map[string]any{}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runChatResumeScenario(t, tc.terminator)
		})
	}
}

func runChatResumeScenario(t *testing.T, terminator e2e.ReplyPart) {
	h := e2e.Start(t, e2e.Options{AgentDir: e2e.TestdataDir("agent-centerdot-companies")})

	// Seed the AgentClass's MCP tools so it reaches Valid (see PingPong).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Each turn is: user text → respond_to_user → (tool_result) → <terminator>,
	// which idles the session. Rule ORDER matters: the respond_to_user
	// tool_result rule is FIRST so that once a reply is delivered the turn ends
	// via the terminator — otherwise the Repeating user-text rules keep
	// re-matching the latest human text (matchUserText walks back past the
	// tool_result) and the agent re-replies forever (MaxTurnsExceeded). All
	// rules Repeating: the harness cold-start can drive a message through the LLM
	// more than once (spec.Prompt replay + the injected inbound), so a
	// non-repeating rule would "no rule matched"-fatal on the replay. The
	// catch-all idles any stray request with agent_work_complete (a reliable
	// terminal for this fixture — EndTurn loops it).
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(terminator).Repeating()
	h.LLM.OnUserMessage("what time").Reply(e2e.RespondToUser("the tides, not the hour")).Repeating()
	h.LLM.OnUserMessage("full moon").Reply(e2e.RespondToUser("no charts fer that")).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	// Turn 1: session-creating message (delivered as spec.Prompt → user turn 0).
	h.SendUserMessage("what time of the day is it?")
	h.ExpectAgentReply(e2e.Contains("the tides, not the hour"))
	e2e.WaitForSessionIdle(t, h)

	// Turn 2: mid-session follow-up on the SAME thread → the inbox path.
	h.SendUserMessage("how many milliseconds to the full moon?")
	h.ExpectAgentReply(e2e.Contains("no charts fer that"))
	e2e.WaitForSessionIdle(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	t.Logf("session: %s/%s", ns, sess.Name)

	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	appender := turn.NewAppender(h.MemStore(), scope)
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-test")

	const q2 = "how many milliseconds to the full moon?"
	const q1 = "what time of the day is it?"

	// Poll until turn 2's drain is durably written: the runner has both the raw
	// "inbox" turn AND a distinct promoted "user" turn for the follow-up, plus
	// the "inbox_done" marker. This is the exact duplication-prone shape the fix
	// deduplicates — without it the assertions below would be vacuous.
	var turns []pkgmemory.Turn
	var inboxIdx, userIdx int
	var sawInboxDone bool
	deadline := time.Now().Add(20 * time.Second)
	for {
		var err error
		turns, err = appender.ReadAll(readCtx)
		require.NoError(t, err)
		inboxIdx, userIdx, sawInboxDone = -1, -1, false
		for _, tn := range turns {
			text := e2e.FirstText(tn.Content)
			switch tn.Role {
			case "inbox":
				if text == q2 {
					inboxIdx = tn.Index
				}
			case "user":
				if text == q2 {
					userIdx = tn.Index
				}
			case "inbox_done":
				sawInboxDone = true
			}
		}
		if inboxIdx >= 0 && userIdx >= 0 && sawInboxDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn 2 never drained to the inbox+user+inbox_done shape:%s", e2e.DumpTurns(turns))
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.NotEqual(t, inboxIdx, userIdx, "the inbox turn and promoted user turn are distinct entries")

	// Render the resumed transcript exactly as the chat replay does. The bug was
	// that a drained message rendered TWICE (raw inbox turn + promoted user
	// turn). The assertions center on the user messages: each human message must
	// appear exactly once, in order, each followed by its reply.
	//
	// NOTE: we assert on user-message counts, not an exact full-transcript match,
	// because the in-process harness re-wakes an idle session and can re-emit the
	// agent's *reply* turn (a harness wake-loop artifact, unrelated to this fix,
	// which only ever duplicates agent turns — never the user's message).
	vs := visibleStrings(turn.VisibleMessages(turns))
	t.Logf("resumed transcript: %v", vs)

	// The two HUMAN messages must each render exactly once — the drained
	// mid-session one is the regression (the bug rendered it twice). User turns
	// are plain text, so an exact "user:<text>" match is safe.
	assert.Equal(t, 1, countExact(vs, "user:"+q1), "first message must render exactly once:\n%v", vs)
	assert.Equal(t, 1, countExact(vs, "user:"+q2), "the drained mid-session message must render exactly once (the bug rendered it twice):\n%v", vs)

	// Replies are matched by substring: the scripted respond_to_user emits both a
	// text block and the tool_use with the same text, so the rendered reply is
	// "<reply>\n\n<reply>" — a scripted-LLM quirk, not a duplicate message.
	assert.GreaterOrEqual(t, countContaining(vs, "agent:", "the tides, not the hour"), 1, "first reply must render")
	assert.GreaterOrEqual(t, countContaining(vs, "agent:", "no charts fer that"), 1, "second reply must render")

	// Conversational order: the first human message precedes the second.
	assert.Less(t, indexOf(vs, "user:"+q1), indexOf(vs, "user:"+q2), "messages must render in order:\n%v", vs)
}

// countExact returns how many elements of xs equal s.
func countExact(xs []string, s string) int {
	n := 0
	for _, x := range xs {
		if x == s {
			n++
		}
	}
	return n
}

// countContaining returns how many elements of xs have the given prefix and
// contain sub.
func countContaining(xs []string, prefix, sub string) int {
	n := 0
	for _, x := range xs {
		if strings.HasPrefix(x, prefix) && strings.Contains(x, sub) {
			n++
		}
	}
	return n
}

// indexOf returns the first index of s in xs, or len(xs) if absent (so an
// absent element sorts last in an ordering comparison).
func indexOf(xs []string, s string) int {
	for i, x := range xs {
		if x == s {
			return i
		}
	}
	return len(xs)
}

func visibleStrings(msgs []turn.VisibleMessage) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role + ":" + m.Text
	}
	return out
}

// TestChatResume_ReplyMatchesLive_NoPreamble drives a REAL runner turn whose
// assistant reply carries model preamble text AND the respond_to_user tool call
// (the shape assembleResponse produces, and real models produce). The live
// surface shows only the respond_to_user reply; this pins that the resumed
// transcript (turn.VisibleMessages — the single source of truth the chat replay
// renders) matches, dropping the preamble rather than showing it (the fidelity
// bug this locks).
func TestChatResume_ReplyMatchesLive_NoPreamble(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: e2e.TestdataDir("agent-centerdot-companies")})

	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// The reply mixes preamble text with the respond_to_user delivery. Rule
	// order + Repeating mirror runChatResumeScenario's rationale.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.OnUserMessage("weather").
		Reply(e2e.Text("let me consult the charts"), e2e.RespondToUser("clear skies ahead")).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	h.SendUserMessage("what is the weather?")
	h.ExpectAgentReply(e2e.Contains("clear skies ahead"))
	e2e.WaitForSessionIdle(t, h)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	appender := turn.NewAppender(h.MemStore(), scope)
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-test")

	turns, err := appender.ReadAll(readCtx)
	require.NoError(t, err)
	vs := visibleStrings(turn.VisibleMessages(turns))
	t.Logf("resumed transcript: %v", vs)

	// The reply must resume EXACTLY as the live surface showed it: the
	// respond_to_user text alone — never the preamble, never doubled. (The
	// harness may re-emit an agent turn on wake, so assert over every agent
	// line rather than a single count.)
	assert.Equal(t, 0, countContaining(vs, "agent:", "let me consult the charts"),
		"preamble text must not leak into the resumed reply:\n%v", vs)
	sawReply := false
	for _, s := range vs {
		if strings.HasPrefix(s, "agent:") {
			assert.Equal(t, "agent:clear skies ahead", s,
				"resumed reply must match what live showed (no preamble, not doubled):\n%v", vs)
			sawReply = true
		}
	}
	assert.True(t, sawReply, "the agent reply must appear in the resumed transcript:\n%v", vs)
}

// TestChatResume_PlanReconstructsToOneFinalCard drives a REAL runner that calls
// update_plan("trip") in turn 1, replies, then calls update_plan("trip") again
// in turn 2 with the item done. On resume, turn.VisibleTimeline must reconstruct
// ONE plan card for "trip" (per-name, first-appearance) carrying the FINAL
// snapshot (status done) — the plan-restoration fidelity guarantee against real
// persisted system_notes.
func TestChatResume_PlanReconstructsToOneFinalCard(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: e2e.TestdataDir("agent-centerdot-companies")})
	h.MCP.OnTool("list_companies", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any { return map[string]any{"results": []any{}} })
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	// Use pending -> done (never pending -> in_progress): the in-process harness
	// only wires plans.Deps.Operations when bundle sessions are present, and a
	// pending -> in_progress transition mints an operation (store.go), which
	// would error here. pending -> done needs no operation, so the plan persists.
	planV1 := map[string]any{"name": "trip", "items": []any{map[string]any{"id": "a", "label": "pack", "status": "pending"}}}
	planV2 := map[string]any{"name": "trip", "items": []any{map[string]any{"id": "a", "label": "pack", "status": "done"}}}

	// A plan update leads to a reply; a reply leads to the turn-ending terminator.
	h.LLM.OnToolResult("update_plan", e2e.AnyResult()).Reply(e2e.RespondToUser("working on it")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.OnUserMessage("start trip").Reply(e2e.ToolUse("update_plan", planV1)).Repeating()
	h.LLM.OnUserMessage("finish trip").Reply(e2e.ToolUse("update_plan", planV2)).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	h.SendUserMessage("start trip")
	h.ExpectAgentReply(e2e.Contains("working on it"))
	e2e.WaitForSessionIdle(t, h)
	h.SendUserMessage("finish trip")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	appender := turn.NewAppender(h.MemStore(), scope)
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-test")

	// Poll until turn 2's update_plan(done) has durably persisted. Both turns'
	// replies reuse "working on it", so ExpectAgentReply can't gate turn 2 (it
	// matches turn 1's echo) — read the persisted memory instead, the same
	// pattern runChatResumeScenario uses. VisibleTimeline must reconstruct
	// EXACTLY ONE "trip" card (per-name, not one per update) carrying the FINAL
	// snapshot (status done).
	var planItems []turn.TimelineItem
	deadline := time.Now().Add(30 * time.Second)
	for {
		turns, err := appender.ReadAll(readCtx)
		require.NoError(t, err)
		planItems = nil
		for _, it := range turn.VisibleTimeline(turns) {
			if it.Kind == "plan" && it.Plan.PlanName == "trip" {
				planItems = append(planItems, it)
			}
		}
		if len(planItems) == 1 && strings.Contains(string(planItems[0].Plan.SnapshotJSON), `"status":"done"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected exactly one 'trip' card with final status done; got %d: %v", len(planItems), planItems)
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.False(t, planItems[0].Plan.Deleted)
	assert.Contains(t, string(planItems[0].Plan.SnapshotJSON), `"status":"done"`, "card shows the FINAL snapshot")
}
