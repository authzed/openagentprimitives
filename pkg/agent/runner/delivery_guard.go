package runner

import (
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// DeliveryNudgeMessage is fed back to the model as an IsError tool_result when
// needsDeliveryNudge fires. It names the specific mistake and the exact
// correction, because a generic "try again" leaves the model free to repeat
// the same shape. Kept in one place so the guard and its tests cannot drift.
const DeliveryNudgeMessage = "BLOCKED: nothing has been delivered to the user this round. " +
	"You wrote your reply as assistant prose, which the channel NEVER shows — the user has seen nothing at all. " +
	"This terminal call was not executed. " +
	"Send the reply NOW by calling respond_to_user with that text (copy it verbatim into the `text` field), " +
	"then call the terminal tool again — both calls together in your next turn."

// markDelivered records that respond_to_user put a message on the channel, so
// the rest of this round's terminal calls pass the guard unchallenged. Called
// from the dispatch goroutines, hence the lock.
func (l *Loop) markDelivered() {
	l.deliveredMu.Lock()
	defer l.deliveredMu.Unlock()
	l.deliveredThisRound = true
}

// beginDeliveryRound reopens the guard when a new inbound user message starts a
// fresh round. Without this reset, one reply early in a long conversation would
// excuse every later round from ever answering.
func (l *Loop) beginDeliveryRound() {
	l.deliveredMu.Lock()
	defer l.deliveredMu.Unlock()
	l.deliveredThisRound = false
}

func (l *Loop) hasDeliveredThisRound() bool {
	l.deliveredMu.Lock()
	defer l.deliveredMu.Unlock()
	return l.deliveredThisRound
}

// roundTerminalTools are the meta tools that end a channel-attached round.
// After either one the runner yields the session to Idle, so a round that
// reaches one without ever calling respond_to_user is the last chance to
// notice that the user got nothing.
//
// ask_parent parks the session too and is deliberately NOT here. It carries
// the child's words to its delegating agent itself — that IS the delivery for
// a conversational child — so nudging it toward respond_to_user first would
// demand a second, redundant copy of the same question, sent over the
// session-to-session channel instead of through the delegation the parent is
// actually waiting on.
var roundTerminalTools = map[string]bool{
	"agent_work_complete": true,
	"await_user_message":  true,
	// return_result is a delegated child's terminal tool, offered in place of
	// agent_work_complete. Classified here for correctness, though the guard
	// cannot currently fire on it: needsDeliveryNudge returns early unless
	// respond_to_user is available, and it is withheld from exactly the
	// sessions this tool is offered to. Listed anyway so that if a delegated
	// child ever regains a human surface, the nudge applies rather than
	// silently not applying.
	"return_result": true,
}

// needsDeliveryNudge reports whether a round-terminating tool call must be
// blocked because the round is about to end having delivered nothing to the
// user.
//
// Instruction alone does not hold: ComposeSystem already tells the model that
// assistant prose is never delivered, and models still write the answer as prose
// and reach straight for a terminal tool. That failure is completely silent —
// prose is dropped, `agent_work_complete`'s summary is audit-only, no error is
// raised, and the session records success while the user watches an empty
// channel — so the rule is enforced structurally at the dispatch choke point.
//
// The guard is deliberately narrow, because a false positive would block a
// legitimate round from ever ending:
//
//   - Only channel-attached sessions. A kubectl-driven session has no channel
//     to deliver to; its output IS the status summary.
//   - Only when respond_to_user is actually in this session's tool list.
//     Demanding a call the model cannot make would block every terminal attempt
//     until the turn budget died — strictly worse than the silence it prevents.
//   - Only when nothing was delivered earlier in the round.
//   - Only when this turn carries non-whitespace prose. With no prose nothing is
//     stranded — an empty terminal turn is a legitimate way to end a round.
//   - Never when the same turn also calls respond_to_user. Dispatch is
//     concurrent, so the sibling call's result is not observable here; its
//     presence in the turn is the reliable signal that delivery was intended.
func needsDeliveryNudge(channelAttached, respondAvailable, deliveredThisRound bool, blocks []llm.ContentBlock, toolName string) bool {
	if !channelAttached || !respondAvailable || deliveredThisRound || !roundTerminalTools[toolName] {
		return false
	}
	stranded := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				stranded = true
			}
		case "tool_use":
			if b.ToolUse != nil && b.ToolUse.Name == "respond_to_user" {
				return false
			}
		}
	}
	return stranded
}
