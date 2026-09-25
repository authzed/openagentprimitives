//go:build e2e

package channel_test

import (
	"context"
	"github.com/authzed/openagentprimitives/test/e2e"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInterrupt_MidTurnQueueAndClickRoundTrip exercises the P2 mid-turn
// interrupt round-trip end-to-end through the real (InProcessRunnerFactory)
// runner, channelsd pipeline, outbound relay, and fake channel kind:
//
//  1. Script the LLM to call the readonly, Cancellable watch_feed MCP tool
//     (see testdata/agent-interruptible-tool/02-mcpserver.yaml:
//     permission.stateImpact: readonly — every MCP tool implements
//     tool.Cancellable via pkg/agent/tool/mcp/dispatch.go's Cancel, which
//     is satisfied by cancelling the per-call ctx). The MCP stub's handler
//     blocks (up to a 15s backstop) so the tool call is GENUINELY in flight
//     when the test sends the mid-turn message — this IS the harness's
//     blocking-tool primitive: mcp_stub.go's OnTool hands the fake MCP
//     server's http handler goroutine directly to the test.
//  2. Wait for the stub to observe the call (⇒ the runner's PatchProgress
//     has already flipped AgentSession.status.phase to Running —
//     PatchProgress runs immediately after the assistant turn is appended
//     and BEFORE dispatchToolUses invokes Execute; see
//     pkg/agent/runner/loop.go and pkg/agent/runner/status.go).
//  3. Send a second, mid-turn user message on the same session/thread.
//     channelsd's pipeline sees Phase==Running and publishes KindEnqueueAck
//     (pkg/channels/channelsd/pipeline/pipeline.go's "Mid-turn enqueue ack" block) —
//     ExpectEnqueueAck asserts it landed.
//  4. Click the interrupt button. The harness's InProcessRunnerFactory
//     wires subscribeFactoryInterruptRequest per session (added alongside
//     this task, mirroring internal/cmd/runner/main.go's
//     `go subscribeInterruptRequest(rootCtx, natsRT, loop, ns, name)`) — it
//     is the piece this task's fake-kind case + interrupt.go helper depend
//     on to make the round-trip observable at all. Without it,
//     KindInterruptRequest would sail into the void and
//     WaitInterruptApplied would never resolve (see interrupt.go's package
//     doc and inprocess_runner_factory.go's subscribeFactoryInterruptRequest
//     for the harness-gap note this task closed).
//  5. loop.Interrupt cancels the in-flight watch_feed call (Cancellable
//     + readonly ⇒ truthfully interruptible per
//     pkg/agent/runner/interrupt.go's interruptibleReason/isInterruptibleTool)
//     and the runner publishes KindInterruptApplied{Outcome:"interrupted"}
//     — asserted via WaitInterruptApplied.
//  6. The runner drains the held queued message and re-asks the LLM; the
//     second scripted rule matches on the queued text and replies, proving
//     the queued message drained rather than being stranded.
//
// This is the reachable subset the task brief anticipated might be needed
// as a fallback ("if the harness has no blocking-tool primitive, assert the
// reachable subset") — it turned out the harness DID have one (MCPStub's
// OnTool handler blocks the request goroutine directly), so this test
// exercises the FULL round-trip including the "interrupted" outcome and the
// queued-message drain, not just the enqueue-ack half.
//
// WHY agent-interruptible-tool AND NOT agent-centerdot-companies — this test
// needs exactly one property from its fixture: a Cancellable MCP tool whose
// declared stateImpact is readonly. centerdot is the SHARED approval fixture
// (23 consumers), and its stateImpact values are chosen by its approval
// model, not by anything this test cares about. When centerdot's
// list_companies moved readonly -> passthrough to close the
// routeViaSessionGrant / wildcard-leaf invariant, this test's tool stopped
// being interruptible and the assertion below flipped to "rejected" — an
// approval-shape change breaking an interrupt test. The dedicated fixture
// (testdata/agent-interruptible-tool) carries no approval flow, no
// routeViaSessionGrant and no wildcard leaf, so that class of change cannot
// reach here again. See that fixture's README.
func TestInterrupt_MidTurnQueueAndClickRoundTrip(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir:       e2e.TestdataDir("agent-interruptible-tool"),
		DefaultTimeout: 20 * time.Second,
		// Must match the single subject 04-spicedbbootstrap.yaml seeds
		// feed:activity-stream#reader for, or watch_feed's readonly check
		// denies and the tool never reaches the stub to be interrupted.
		DefaultUser: "user@example.com",
	})

	release := make(chan struct{})
	h.MCP.OnTool("watch_feed", func(_ map[string]any) any {
		// Blocks until the test explicitly releases it (right after
		// asserting the interrupt landed) or a 15s backstop fires. The
		// backstop bounds MCPStub.Close's cleanup wait (it blocks until
		// outstanding requests complete) even if an earlier assertion
		// fails this test before the explicit release is reached.
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
	// Repeating because a cold-start replay can re-ask the LLM with the
	// same first user message (mirrors TestConversation_PingPong's
	// OnUserMessage("ping") rule).
	// The LLM-facing tool name is prefixed with the mcpServer name
	// (slowfeed). The MCP stub's OnTool above uses the bare upstream name;
	// the runner strips the prefix before forwarding.
	h.LLM.OnUserMessage(firstMsg).Reply(
		e2e.ToolUse("slowfeed_watch_feed", map[string]any{
			"operation_id": "op-interrupt-1", "_reason": "watching the feed",
			"args": map[string]any{"sinceSeconds": 60},
		}),
	).Repeating()
	// Rule 2: fires on the post-interrupt continuation, once loop.Interrupt
	// has cancelled watch_feed and drainAfterInterrupt has spliced the
	// held queued message in as the new latest user turn (see
	// pkg/agent/runner/loop.go's drainAfterInterrupt / dispatchToolUses
	// call site). Proves the queued message drained rather than being lost.
	h.LLM.OnUserMessage(queuedMsg).Reply(
		e2e.RespondToUser("handled the queued question"),
	)
	// Rule 3: end the turn once respond_to_user reports "delivered".
	// Otherwise the runner re-asks the LLM what to do next, no rule
	// matches, and the runner goroutine trips t.Fatalf from under
	// ExpectAgentReply (mirrors the centerdot list_companies happy-path
	// scenario's third rule for the same reason).
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	h.SendUserMessage(firstMsg)

	// Synchronize on the tool call actually reaching the MCP stub: only
	// then is watch_feed genuinely in flight (and, per PatchProgress's
	// call ordering, the session is already Running).
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
	// watch_feed is Cancellable (every MCP tool is, via dispatch.go's
	// Cancel-by-ctx-cancellation) AND readonly, and it was the only
	// in-flight work at click time, so currentInterruptibility truthfully
	// cancels it — Outcome MUST be "interrupted", not "rejected" (see
	// pkg/agent/runner/interrupt.go's currentInterruptibility /
	// isInterruptibleTool). A flip to "rejected" here means either the
	// tool stopped being truthfully cancellable or nothing was in flight
	// at click time — both would be real regressions, not test flakiness.
	// If it ever flips, check watch_feed's stateImpact in
	// testdata/agent-interruptible-tool/02-mcpserver.yaml first: only
	// stateless/readonly are interruptible.
	assert.Equal(t, "interrupted", applied.Outcome, "interrupt outcome")
	assert.Equal(t, interrupt.Ack().RequestID, applied.RequestID, "RequestID round-trips")

	close(release) // Let the (now client-side-cancelled) blocked handler return.

	got := h.ExpectAgentReply(e2e.Contains("handled the queued question"))
	t.Logf("agent replied after drain: %q", got.Text)
}
