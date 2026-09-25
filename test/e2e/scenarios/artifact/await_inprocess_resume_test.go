//go:build e2e

package artifact_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	pkgmemory "github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/turn"
)

// TestAwaitUserMessage_InProcessResumeDrainsFollowup exercises the IN-PROCESS
// await_user_message park/resume path — the one a live conversation actually
// takes and the one a budget-blowup incident occurred on, yet
// the one no e2e test covered (TestChatResume_NoDuplicateDrainedMessage runs
// await with IdleTTL unset, so it idle-exits and respawns instead of parking).
//
// With AwaitIdleTTL > 0 the harness wires a live InboundCh fed by channelsd's
// wakeup, so await_user_message genuinely BLOCKS in the running loop. The test
// then sends a follow-up over the real inbound path and asserts:
//
//  1. the follow-up resumes the parked await in-process (no respawn) and the
//     agent replies to it — proving the runner's drain delivered the message as
//     a real user turn without the agent having to hunt for it;
//  2. the follow-up is promoted to a distinct "user" turn with a paired
//     "inbox_done" marker (not stranded);
//  3. the await resume result recorded in memory is the neutral "acknowledged"
//     ack, and NOTHING tells the agent to "check memory for the new turn" — the
//     fossil instruction that turned a drain-miss into a query_memory transcript
//     dump. This is the regression guard against that string ever growing back.
func TestAwaitUserMessage_InProcessResumeDrainsFollowup(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: e2e.TestdataDir("agent-centerdot-companies"),
		// Park in-process long enough to send the follow-up; the resume happens
		// in milliseconds, well before this fires.
		AwaitIdleTTL: 30 * time.Second,
	})

	// Seed the AgentClass's MCP tools so it reaches Valid (see PingPong / the
	// sibling chat-resume test).
	h.MCP.OnTool("list_companies", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.MCP.OnTool("list_contacts_for_company", func(_ map[string]any) any {
		return map[string]any{"results": []any{}}
	})
	h.WaitForAgentClassValid("centerdot-companies", 30*time.Second)

	const q1 = "scope my request first"
	const q2 = "architects, print-ready HTML"

	// Turn 1: the session-creating message → the agent immediately yields with
	// await_user_message, which now PARKS in-process. Turn 2: once the follow-up
	// drains in as a user turn, the agent replies and completes. All Repeating:
	// the cold-start can drive a message through the LLM more than once. Rule
	// order: the respond_to_user tool_result terminator is placed before the
	// catch-all so a delivered reply ends the turn cleanly.
	h.LLM.OnUserMessage(q1).Reply(e2e.ToolUse("await_user_message", map[string]any{})).Repeating()
	h.LLM.OnUserMessage("architects").Reply(e2e.RespondToUser("one-pager for architects — done")).Repeating()
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()
	h.LLM.On(func(llm.Request) bool { return true }).
		Reply(e2e.ToolUse("agent_work_complete", map[string]any{"summary": "done"})).Repeating()

	// Turn 1: session-creating message → the agent parks in await_user_message.
	h.SendUserMessage(q1)
	waitForSessionRunning(t, h)

	// Turn 2: mid-session follow-up on the SAME thread. This travels the real
	// inbound path — channelsd appends the "inbox" turn and publishes the wakeup
	// that the harness forwards into the parked await's InboundCh, resuming the
	// loop IN-PROCESS. The reply proves the drained message reached the model.
	h.SendUserMessage(q2)
	h.ExpectAgentReply(e2e.Contains("one-pager for architects"))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	scope := pkgmemory.Scope{Kind: "session", ID: ns + "/" + sess.Name}
	appender := turn.NewAppender(h.MemStore(), scope)
	readCtx := pkgmemory.WithSystemApproval(ctx, "e2e-test")

	// Poll until the follow-up has durably drained to the inbox + promoted-user +
	// inbox_done shape (the drain is async relative to ExpectAgentReply).
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
			switch tn.Role {
			case "inbox":
				if e2e.FirstText(tn.Content) == q2 {
					inboxIdx = tn.Index
				}
			case "user":
				if e2e.FirstText(tn.Content) == q2 {
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
			t.Fatalf("follow-up never drained to inbox+user+inbox_done shape (in-process resume path):%s", e2e.DumpTurns(turns))
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.NotEqual(t, inboxIdx, userIdx, "the inbox turn and its promoted user turn are distinct entries (message delivered, not stranded)")

	// The await resume result is the neutral ack, and the fossil instruction is
	// gone everywhere in the transcript.
	contents := allContentText(turns)
	var sawAck bool
	for _, c := range contents {
		if strings.Contains(c, "check memory for the new turn") {
			t.Errorf("transcript still contains the fossil await instruction 'check memory for the new turn':\n%s", c)
		}
		if strings.Contains(c, "acknowledged") {
			sawAck = true
		}
	}
	assert.True(t, sawAck, "the await_user_message resume result must be recorded as the neutral 'acknowledged' ack")
}

// waitForSessionRunning blocks until the single AgentSession reaches phase
// Running — the phase an in-process await_user_message park holds (OnAwaitYield
// keeps the pod alive and the phase Running while parked on the user).
func waitForSessionRunning(t *testing.T, h *e2e.Harness) {
	t.Helper()
	ns := h.Namespace()
	sess := e2e.FindOnlyAgentSession(t, h, ns)
	e2e.Eventually(t, 30*time.Second, func() bool {
		var s spiceboxv1alpha1.AgentSession
		if err := h.K8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: sess.Name}, &s); err != nil {
			return false
		}
		return s.Status.Phase == spiceboxv1alpha1.AgentSessionPhaseRunning
	}, "session reaches Running (parked in await)")
}

// allContentText flattens every text block and tool_result payload across the
// transcript into a slice of strings, for substring assertions.
func allContentText(turns []pkgmemory.Turn) []string {
	var out []string
	for _, tn := range turns {
		for _, b := range tn.Content {
			if b.Text != "" {
				out = append(out, b.Text)
			}
			if b.ToolResult != nil && b.ToolResult.Content != "" {
				out = append(out, b.ToolResult.Content)
			}
		}
	}
	return out
}
