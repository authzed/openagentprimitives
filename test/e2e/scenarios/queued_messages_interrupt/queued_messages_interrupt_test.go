//go:build e2e

// Package queued_messages_interrupt_test is the end-to-end scenario for the P2
// mid-turn "queue + Interrupt & Send Now" round-trip, exercised over the
// LEGACY fake queued_messages path.
//
// WHY THE LEGACY (not the generic Interaction) PATH HERE — read this before
// "fixing" the scenario to assert interaction_request(queued_messages):
//
// Slice B migrated queued_messages onto the generic Interaction model, but the
// producer gate is deliberately SLACK-SCOPED (Task 6): the channelsd pipeline
// publishes interaction_request(queued_messages) ONLY when
// InputChannel.Kind == "slack"; every other kind keeps the legacy KindEnqueueAck
// until its own migration slice lands (pkg/channels/channelsd/pipeline/pipeline.go, the
// "Mid-turn enqueue ack" block). This harness drives a kind: fake session
// (testdata/agent-interruptible-tool), so it takes the LEGACY branch:
//
//   - mid-turn inbound while Running → channelsd publishes KindEnqueueAck on OUT
//     → fake queued_messages sub-channel sender → Driver.enqueueAcks
//     (ExpectEnqueueAck).
//   - Interrupt.Click publishes KindInterruptRequest directly on IN (NOT a
//     generic interaction_decision) → the in-process runner factory's
//     subscribeFactoryInterruptRequest → loop.Interrupt → KindInterruptApplied
//     on OUT → fake queued_messages sub-channel sender → Driver.interruptApplieds
//     (WaitInterruptApplied). The Slack-scoped interrupt_applied bridge NO-OPs
//     for a fake session (the double-fire guard), and the fake sender consumes
//     KindInterruptApplied directly.
//
// This scenario is the regression guard that Tasks 6–8 did NOT break the
// builtin/local/fake LEGACY path while staging the Slack migration.
//
// WHERE THE SLACK-SPECIFIC INVARIANT IS COVERED — the Slack round-trip
// (interaction_request(queued_messages) → interaction_decision → Suppressed →
// interrupt_request → runner interrupt_applied → bridge →
// interaction_applied, asserting EXACTLY ONE applied / no double-fire) is not
// e2e-reachable via the fake kind (a slack-kind fake session is infeasible:
// the real "slack" kind is registered process-wide and the channel-kind
// registry panics on a duplicate registration; a real slack-kind session
// routes its interactions through the real slack sender + fakeslack, not the
// fake Driver, and needs heavy new harness infra to drive mid-turn with thread
// correlation). It IS covered, at every seam, by unit tests:
//
//   - pkg/channels/channelsd/pipeline/queued_interrupt_test.go
//     (TestDecideQueuedInterrupt: exactly ONE interrupt_request published,
//     Outcome{Suppressed:true} with no synchronous applied; TestBindQueuedInterruptHandler).
//   - pkg/channels/channelsd/pipeline/interaction_decision_test.go (the Suppressed path:
//     the generic pipe skips the synchronous interaction_applied).
//   - internal/cmd/channelsd/interrupt_applied_bridge_test.go
//     (TestInterruptAppliedBridge_SlackSession_RepublishesAsInteractionApplied:
//     EXACTLY ONE interaction_applied for a Slack session;
//     TestInterruptAppliedBridge_NonSlackSession_NoOp: ZERO publishes for
//     builtin/local/bento/"" — the double-fire guard).
//
// No real names: user@example.com is fictional per AGENTS.md.
package queued_messages_interrupt_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/test/e2e"
)

// TestQueuedMessagesInterrupt_LegacyFakeRoundTrip exercises the mid-turn
// queue → Interrupt & Send Now round-trip end-to-end through the real
// (InProcessRunnerFactory) runner, channelsd pipeline, outbound relay, and fake
// channel kind's legacy queued_messages sub-channel:
//
//  1. Script the LLM to call the readonly, Cancellable watch_feed MCP tool.
//     The MCP stub's handler blocks (up to a 15s backstop) so the tool call is
//     GENUINELY in flight when the test sends the mid-turn message — the
//     harness's blocking-tool primitive (mcp_stub.go's OnTool hands the fake
//     MCP server's http handler goroutine directly to the test).
//  2. Wait for the stub to observe the call (⇒ PatchProgress has already flipped
//     AgentSession.status.phase to Running).
//  3. Send a second, mid-turn user message on the same session/thread.
//     channelsd's pipeline sees Phase==Running and (kind=fake ⇒ legacy branch)
//     publishes KindEnqueueAck — ExpectEnqueueAck asserts it landed.
//  4. Click the interrupt button. The InProcessRunnerFactory wires
//     subscribeFactoryInterruptRequest per session; loop.Interrupt cancels the
//     in-flight watch_feed call and the runner publishes
//     KindInterruptApplied{Outcome:"interrupted"} — asserted via
//     WaitInterruptApplied.
//  5. The runner drains the held queued message and re-asks the LLM; the second
//     scripted rule matches on the queued text and replies, proving the queued
//     message drained rather than being stranded.
//
// WHY agent-interruptible-tool AND NOT agent-centerdot-companies — the only
// fixture property this scenario needs is a Cancellable MCP tool whose declared
// stateImpact is readonly, because stateImpact is ALSO the runtime
// interruptibility input (pkg/agent/runner/interrupt.go's interruptibleReason:
// only stateless/readonly are interruptible). centerdot is the shared APPROVAL
// fixture with 23 consumers; when its list_companies moved readonly ->
// passthrough to close the routeViaSessionGrant / wildcard-leaf invariant, this
// scenario's tool silently stopped being cancellable and step 4's outcome
// flipped to "rejected". The dedicated fixture has no approval flow, no
// routeViaSessionGrant and no wildcard leaf, so approval-shape changes can no
// longer reach here. See testdata/agent-interruptible-tool/README.md.
func TestQueuedMessagesInterrupt_LegacyFakeRoundTrip(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       "../../testdata/agent-interruptible-tool",
		DefaultTimeout: 20 * time.Second,
		// Must match the single subject 04-spicedbbootstrap.yaml seeds
		// feed:activity-stream#reader for, or watch_feed's readonly check
		// denies and the tool never reaches the stub to be interrupted.
		DefaultUser: "user@example.com",
	})

	release := make(chan struct{})
	h.MCP.OnTool("watch_feed", func(_ map[string]any) any {
		// Blocks until the test explicitly releases it (right after asserting
		// the interrupt landed) or a 15s backstop fires. The backstop bounds
		// MCPStub.Close's cleanup wait even if an earlier assertion fails the
		// test before the explicit release is reached.
		select {
		case <-release:
		case <-time.After(15 * time.Second):
		}
		return map[string]any{"items": []any{}}
	})
	h.WaitForAgentClassValid("interruptible-tool", 30*time.Second)
	// Block on SpiceDBBootstrap too: watch_feed is readonly, so the runner
	// runs an authz Check before dispatching. That Check needs the seeded
	// feed:activity-stream#reader tuple live, or the call is denied and never
	// reaches the MCP stub to be interrupted.
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	const (
		firstMsg  = "look up companies"
		queuedMsg = "urgent unrelated question"
	)

	// Rule 1: the first turn calls the blocking, readonly watch_feed tool.
	// Repeating because a cold-start replay can re-ask the LLM with the same
	// first user message. The LLM-facing tool name is prefixed with the
	// mcpServer name (slowfeed); the MCP stub's OnTool uses the bare upstream
	// name, and the runner strips the prefix before forwarding.
	h.LLM.OnUserMessage(firstMsg).Reply(
		e2e.ToolUse("slowfeed_watch_feed", map[string]any{
			"operation_id": "op-queued-1", "_reason": "watching the feed",
			"args": map[string]any{"sinceSeconds": 60},
		}),
	).Repeating()
	// Rule 2: fires on the post-interrupt continuation, once loop.Interrupt has
	// cancelled watch_feed and drainAfterInterrupt has spliced the held
	// queued message in as the new latest user turn. Proves the queued message
	// drained rather than being lost.
	h.LLM.OnUserMessage(queuedMsg).Reply(
		e2e.RespondToUser("handled the queued question"),
	)
	// Rule 3: end the turn once respond_to_user reports "delivered".
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage(firstMsg)

	// Synchronize on the tool call actually reaching the MCP stub: only then is
	// watch_feed genuinely in flight (and, per PatchProgress's call
	// ordering, the session is already Running).
	require.Eventually(t, func() bool {
		for _, c := range h.MCP.Calls() {
			if c.Name == "watch_feed" {
				return true
			}
		}
		return false
	}, 10*time.Second, 100*time.Millisecond, "watch_feed call never reached the MCP stub")

	h.SendUserMessage(queuedMsg)

	interrupt := h.ExpectEnqueueAck(e2e.ForEnqueueRequester("user@example.com"))
	require.NotNil(t, interrupt, "enqueue-ack handle")
	assert.NotEmpty(t, interrupt.Ack().RequestID, "enqueue ack RequestID")

	interrupt.Click()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer waitCancel()
	applied, err := interrupt.WaitInterruptApplied(waitCtx)
	require.NoError(t, err, "WaitInterruptApplied")
	// watch_feed is Cancellable AND readonly, and it was the only in-flight
	// work at click time, so currentInterruptibility truthfully cancels it —
	// Outcome MUST be "interrupted", not "rejected". If it ever flips, check
	// watch_feed's stateImpact in
	// testdata/agent-interruptible-tool/02-mcpserver.yaml first: only
	// stateless/readonly are interruptible.
	assert.Equal(t, "interrupted", applied.Outcome, "interrupt outcome")
	assert.Equal(t, interrupt.Ack().RequestID, applied.RequestID, "RequestID round-trips")

	close(release) // Let the (now client-side-cancelled) blocked handler return.

	got := h.ExpectAgentReply(e2e.Contains("handled the queued question"))
	t.Logf("agent replied after drain: %q", got.Text)
}
