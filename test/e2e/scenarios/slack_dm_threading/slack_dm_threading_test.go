//go:build e2e

// Package slack_dm_threading_test is Plan-2 Task 6 — the payoff. Unlike
// every other e2e scenario (which drives the in-process kind: fake
// transport), these tests drive a real Slack DM/mention through the REAL
// slack channel kind:
//
//	fakeslack.InjectDM/InjectMention (socketmode.Event)
//	  → real slackListener.handle → handleDM/handleChannelMessage
//	  → channelsd pipeline.Deliver (identity + authz + session correlation)
//	  → InProcessRunnerFactory (ScriptedLLM) → respond_to_user
//	  → NATS out.user_message → outbound relay
//	  → real slackSender.Send (reads the LastInboundTS annotation)
//	  → fakeslack (records thread_ts)
//
// and prove the agent's reply threads under the right anchor — a DM
// message's own ts, or a channel mention's own ts — rather than some
// stale/shared thread root. TestSlack_MultiTurnDM_EachReplyThreadsUnderItsTurn
// is the sharpest test of this: it proves each turn in an ongoing DM gets
// its OWN anchor, which is the crux of the per-turn LastInboundTS fix this
// plan is about.
//
// Identity gotcha (see SlackFake's godoc): the real listener only trusts a
// Slack profile email as the canonical SpiceDB subject for a full member of
// the bot's own installed team (fakeslack's AuthTestContext reports
// TeamID "T-test"). Every test below seeds its sending user with that
// TeamID before driving any inbound, or the message is attributed to an
// untrusted synthetic identity and nothing here would route.
//
// No real names — "user@example.com" is fictional per AGENTS.md, and is
// the same default identity test/e2e's Options.DefaultUser and every
// centerdot fixture's SpiceDBBootstrap use.
package slack_dm_threading_test

import (
	"context"
	"strings"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	spiceboxv1alpha1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/slack/fakeslack"
	"github.com/authzed/openagentprimitives/test/e2e"
)

const (
	slackDMAgentDir = "../../testdata/agent-slack-dm"
	slackDMAgentCls = "slack-dm-agent"

	// humanUserID/humanEmail are the seeded sender identity shared by all
	// three scenarios below (see seedHumanUser).
	humanUserID = "U-human"
	humanEmail  = "user@example.com"
)

// seedHumanUser registers the DM/mention sender with the harness's shared
// fakeslack.Client BEFORE any inbound is driven. TeamID MUST equal
// fakeslack's AuthTestContext TeamID ("T-test") — the real listener's
// emailTrusted gate (pkg/channels/channelkinds/slack/listener.go) only honors the
// profile email for a full member of the bot's own installed workspace;
// anything else (foreign team, bot, guest, deleted) falls back to an
// unforgeable synthetic identity that would never match this fixture's
// expected canonical subject ("user@example.com").
func seedHumanUser(h *e2e.Harness) {
	h.SlackFake().SeedUser(&slackapi.User{
		ID:     humanUserID,
		TeamID: "T-test",
		Profile: slackapi.UserProfile{
			Email: humanEmail,
		},
	})
}

// waitForSessionIdle polls until every AgentSession in the harness's
// namespace reports status.phase=="Idle", or timeout elapses. Needed
// between two DM turns in the SAME session:
//
// channelsd's pipeline.Deliver classifies a correlated session's
// continuation disposition by reading its status.Phase off the
// CONTROLLER-CACHED client (mgr.GetClient(), an informer cache) — a
// different, separately-converging cache from the harness's own direct
// h.K8s client. Turn 1's respond_to_user reply becomes visible in
// fakeslack (this test's signal to send turn 2) the moment the sender
// posts it, which is BEFORE the runner finishes its own turn and writes
// status.Phase=Idle, and possibly before that write has propagated to the
// pipeline's cache even once it IS written. If turn 2's inbound is
// delivered while the pipeline still observes a stale non-Idle phase,
// respawnOnWake(phase) (pkg/channels/channelsd/pipeline/pipeline.go) returns false
// — no wake-requested-at annotation is written — and the turn is silently
// stranded: the memory append + NATS wakeup both succeed, but turn 1's
// per-turn runner goroutine has already exited, so nothing is listening
// for the wakeup and nothing ever respawns a runner for it. No error
// surfaces anywhere; the session just sits Idle forever with turn 2's
// text appended but never processed. Waiting here for our direct read to
// observe Idle, plus a short settle buffer for the pipeline's cache to
// catch up, closes that window. This is a genuine race in the
// continuation path (independent of anything this plan's per-turn-anchor
// fix touches) — the fix here is test-side patience, not a production
// change, mirroring how contacts_multiturn_approve_then_deny budgets 30s
// for cold-runner respawns rather than changing the respawn path itself.
func waitForSessionIdle(t *testing.T, h *e2e.Harness, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var list spiceboxv1alpha1.AgentSessionList
		if err := h.K8s.List(context.Background(), &list); err == nil && len(list.Items) > 0 {
			allIdle := true
			for i := range list.Items {
				if list.Items[i].Status.Phase != spiceboxv1alpha1.AgentSessionPhaseIdle {
					allIdle = false
					break
				}
			}
			if allIdle {
				// Settle buffer: the pipeline's own informer-cache read of
				// this same Idle transition can lag our direct read by a
				// short interval. See godoc above.
				time.Sleep(500 * time.Millisecond)
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("waitForSessionIdle: no AgentSession reached Idle within %s", timeout)
}

// waitForReplyContaining polls the fake Slack conversation tree until a
// message threaded under rootTS contains substr, or timeout elapses.
//
// Unlike a DM turn (exactly one threaded reply — see
// (*Harness).ExpectSlackThreadedReply), a brand-new channel-mention thread
// gets a listener-posted "started" banner (threadBootstrapPlan/postStarter
// in pkg/channels/channelkinds/slack/listener.go) threaded under the SAME anchor
// BEFORE the agent's real reply lands — so a strict "exactly one reply"
// wait would return the banner instead of racing it out. This helper finds
// the specific reply carrying the agent's actual text among however many
// threaded messages have landed under the anchor so far.
func waitForReplyContaining(t *testing.T, h *e2e.Harness, channelID, rootTS, substr string, timeout time.Duration) fakeslack.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range h.SlackFake().Replies(channelID, rootTS) {
			if strings.Contains(m.Text, substr) {
				return m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	replies := h.SlackFake().Replies(channelID, rootTS)
	t.Fatalf("waitForReplyContaining: no reply containing %q under %s in %s within %s (top-level=%d, replies=%d)",
		substr, rootTS, channelID, timeout, len(h.SlackTopLevel(channelID)), len(replies))
	return fakeslack.Message{}
}

// TestSlack_DM_ReplyThreadsUnderUserMessage drives a single real Slack DM
// end to end and proves the agent's reply threads under the user's own
// message ts — the core "reply anchors to the turn that prompted it"
// invariant, exercised through the REAL slack kind rather than the fake one.
func TestSlack_DM_ReplyThreadsUnderUserMessage(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: slackDMAgentDir})
	h.WaitForAgentClassValid(slackDMAgentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	seedHumanUser(h)

	h.LLM.OnUserMessage("ping").Reply(e2e.RespondToUser("pong"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	// REAL slack listener → handleDM (stamps LastInboundTS) → pipeline →
	// runner → respond_to_user → relay → REAL slack sender (reads the
	// annotation) → fakeslack.
	rootTS := h.SendSlackDM(humanUserID, "D-human", "ping")
	reply := h.ExpectSlackThreadedReply(t, "D-human", rootTS, 30*time.Second)

	assert.Contains(t, reply.Text, "pong")
	assert.Equal(t, rootTS, reply.ThreadTS, "reply must thread under the user's DM message")
	assert.Len(t, h.SlackTopLevel("D-human"), 1, "only the user's message is top-level (no orphan reply)")
	h.AssertAllRulesConsumed()
}

// TestSlack_ChannelMention_ThreadsUnderMention is the channel-@mention
// regression: guards the pre-existing (non-DM) threading behavior end to
// end through the real slack kind, including the listener's one-time
// "started" banner for a brand-new bot-rooted thread.
func TestSlack_ChannelMention_ThreadsUnderMention(t *testing.T) {
	h := e2e.Start(t, e2e.Options{AgentDir: slackDMAgentDir})
	h.WaitForAgentClassValid(slackDMAgentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	seedHumanUser(h)

	h.LLM.OnUserMessage("hello team").Reply(e2e.RespondToUser("hello back"))
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).Reply(e2e.EndTurn())

	// REAL slack listener → handleChannelMessage (new bot-rooted thread:
	// posts the one-time starter banner threaded under the mention, then
	// stamps LastInboundTS) → pipeline → runner → respond_to_user → relay
	// → REAL slack sender → fakeslack.
	rootTS := h.SendSlackMention(humanUserID, "C-team", "hello team")
	reply := waitForReplyContaining(t, h, "C-team", rootTS, "hello back", 30*time.Second)

	assert.Equal(t, rootTS, reply.ThreadTS, "reply must thread under the mention's own ts")
	assert.Len(t, h.SlackTopLevel("C-team"), 1, "only the mention itself is top-level (starter banner + reply are both threaded)")
	h.AssertAllRulesConsumed()
}

// TestSlack_MultiTurnDM_EachReplyThreadsUnderItsTurn is the sharpest proof
// of the per-turn anchor: two DM turns in the SAME ongoing session (same
// channelKey "dm:<user>", hence the same AgentSession), each turn's reply
// must thread under THAT turn's own message ts — not the first turn's ts,
// not a shared "assistant thread" root. That distinction is exactly what
// the per-turn LastInboundTS stamp (this plan's core fix) is for.
func TestSlack_MultiTurnDM_EachReplyThreadsUnderItsTurn(t *testing.T) {
	h := e2e.Start(t, e2e.Options{
		AgentDir: slackDMAgentDir,
		// Second turn's cold-runner-idle→wake respawn can exceed the 10s
		// default under suite load; match the other multi-turn e2e
		// scenarios' 30s budget (see contacts_multiturn_approve_then_deny).
		DefaultTimeout: 30 * time.Second,
	})
	h.WaitForAgentClassValid(slackDMAgentCls, 30*time.Second)
	h.WaitForSpiceDBBootstrap(30 * time.Second)

	seedHumanUser(h)

	h.LLM.OnUserMessage("turn one").Reply(e2e.RespondToUser("reply one"))
	h.LLM.OnUserMessage("turn two").Reply(e2e.RespondToUser("reply two"))
	// respond_to_user's own "delivered" tool_result asks the LLM what to do
	// next on EVERY turn, so this rule must survive across both turns.
	h.LLM.OnToolResult("respond_to_user", e2e.AnyResult()).
		Reply(e2e.EndTurn()).Repeating()

	rootTS1 := h.SendSlackDM(humanUserID, "D-human", "turn one")
	reply1 := h.ExpectSlackThreadedReply(t, "D-human", rootTS1, 30*time.Second)
	assert.Contains(t, reply1.Text, "reply one")
	assert.Equal(t, rootTS1, reply1.ThreadTS, "turn 1's reply must thread under turn 1's own message ts")

	// Let turn 1 fully settle to Idle (see waitForSessionIdle's godoc) before
	// sending turn 2 — otherwise this test can race the pipeline's own
	// continuation-classification cache and silently strand turn 2 forever.
	waitForSessionIdle(t, h, 10*time.Second)

	rootTS2 := h.SendSlackDM(humanUserID, "D-human", "turn two")
	require.NotEqual(t, rootTS1, rootTS2, "each DM turn must get its own distinct anchor ts")
	reply2 := h.ExpectSlackThreadedReply(t, "D-human", rootTS2, 30*time.Second)
	assert.Contains(t, reply2.Text, "reply two")
	assert.Equal(t, rootTS2, reply2.ThreadTS, "turn 2's reply must thread under turn 2's own message ts — NOT turn 1's")

	// Turn 1's thread must not have picked up a second (turn-2) reply.
	assert.Len(t, h.SlackFake().Replies("D-human", rootTS1), 1, "turn 1's thread must still carry exactly its own reply")
	assert.Len(t, h.SlackTopLevel("D-human"), 2, "both user DMs are top-level; no orphan replies")
	h.AssertAllRulesConsumed()
}
