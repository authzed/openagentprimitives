// pkg/agent/runner/resume_park.go
//
// The "resumed with nothing to do" case: a runner that came back for a reason
// other than a message.
package runner

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	lifecyclecore "github.com/authzed/openagentprimitives/pkg/agent/session/lifecycle"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/lifecycle"
)

// awaitUserMessageToolName is the meta tool the agent calls to park. The loop
// borrows it by name for the resume case; see parkUntilInbound.
const awaitUserMessageToolName = "await_user_message"

// resumeShape names why a resume found nothing to do, for the park's log line
// and its exit. resumeHasWork is the zero value: take the turn.
type resumeShape string

const (
	resumeHasWork resumeShape = ""
	// resumeAssistantLast: the transcript ends with the agent's own message.
	resumeAssistantLast resumeShape = "assistant-last"
	// resumeCompletedRound: the transcript ends with a round the agent closed
	// through a recorded, successful agent_work_complete.
	resumeCompletedRound resumeShape = "completed-round"
)

// resumedWithNothingToDo reports whether this Run has nothing to respond to,
// and which shape of transcript made it so.
//
// hadInitialPrompt distinguishes a RESUME from a first boot. A first boot
// always has something to say — its opening prompt — and must never park.
// Past that, the transcript has nothing outstanding in two shapes:
//
//   - The last message is the ASSISTANT's. Calling the model then is both
//     pointless and invalid (providers reject a conversation ending in an
//     assistant message).
//   - completedRound (see completedRoundAtRest, and the caller's gate on which
//     sessions it applies to), nothing was appended since the replay
//     (len(messages) == replayed), and the last message carries only tool
//     results. The live loop parks Idle the moment agent_work_complete
//     succeeds and never shows the model that result, so a resume that finds
//     the same transcript has no more to do than the live loop had.
//
// Anything else — a just-drained inbox turn, a refused completion handed back
// as an error, a tool batch a prior run died before answering — IS outstanding
// and the loop must run.
//
// It checks the message shape rather than "did drainInbox return anything"
// because the question is what the model would be sent, which the messages
// themselves answer — so a future path that appends something else for the model
// to act on is covered without knowing about it.
func resumedWithNothingToDo(hadInitialPrompt, completedRound bool, replayed int, messages []llm.Message) resumeShape {
	if !hadInitialPrompt || len(messages) == 0 {
		return resumeHasWork
	}
	last := messages[len(messages)-1]
	if last.Role == "assistant" {
		return resumeAssistantLast
	}
	if completedRound && len(messages) == replayed && isToolResultRelay(last) {
		return resumeCompletedRound
	}
	return resumeHasWork
}

// completedRoundAtRest reports whether the durable transcript ends with a
// round the agent closed through agent_work_complete, every step of which
// actually ran. When it does, completedAt is the memory Index of the assistant
// turn that closed it — what completedRoundExitEvent matches the lifecycle log
// against.
//
// Deliberately stricter than resumedWithNothingToDo's assistant-last check,
// which only has to see who spoke last:
//
//   - EVERY tool_use in the last assistant turn has a RECORDED result in prior.
//     A runner that died between appending the assistant turn and recording
//     its results left the batch unexecuted — replay synthesizes "no result
//     recorded" answers for it (repairOrphanToolUses), but those are not
//     evidence anything ran, and parking there would silently drop the
//     reply, the goal report, the completion itself.
//   - An agent_work_complete in that turn has a recorded result that is not an
//     error: a refused completion left the round open. One success is enough.
//     A sibling's error does not reopen the round, and neither does a second
//     agent_work_complete in the same batch that failed — the live loop parks
//     on a successful completion whatever else the batch returned.
//   - Nothing but tool results was placed after that assistant turn. Any other
//     user turn (an inbound already drained, an authorless notice) is
//     something the model has not answered yet.
func completedRoundAtRest(prior []memory.Turn) (completedAt int, ok bool) {
	last := -1
	for i, t := range prior {
		if t.Role == "assistant" && (last < 0 || t.Index > prior[last].Index) {
			last = i
		}
	}
	if last < 0 || prior[last].Refused {
		return 0, false
	}
	a := prior[last]
	results := map[string]*memory.ToolResultBlock{}
	for _, t := range prior {
		if t.Role != "user" || t.Index <= a.Index {
			continue
		}
		for _, b := range t.Content {
			if b.Type != "tool_result" || b.ToolResult == nil {
				return 0, false
			}
			results[b.ToolResult.ToolUseID] = b.ToolResult
		}
	}
	completed := false
	for _, b := range a.Content {
		if b.Type != "tool_use" || b.ToolUse == nil {
			continue
		}
		r, recorded := results[b.ToolUse.ID]
		if !recorded {
			return 0, false
		}
		if b.ToolUse.Name == "agent_work_complete" && !r.IsError {
			completed = true
		}
	}
	return a.Index, completed
}

// completedRoundExitEvent picks the lifecycle event a completed-round park
// records when it expires: IdleYield when the round's AgentWorkComplete is
// already in the signed log, AgentWorkComplete itself when it is not.
//
// It can be missing because the live loop appends the completion's tool_result
// turn BEFORE it emits AgentWorkComplete. A runner that died between the two
// left a transcript completedRoundAtRest accepts and a log with no completion
// in it, and this exit is the last place that round's completion can still be
// written. When the event is there, writing it again would record a completion
// that did not happen.
//
// "Already recorded" is an AgentWorkComplete keyed at or after completedAt, the
// assistant turn that closed the round. The live loop keys it at that turn; an
// earlier resume that backfilled it keyed it at its own replay base, which is
// later. Nothing later than that turn belongs to another round, because the
// transcript has no later assistant turn. An entry written before ordering
// keys existed cannot be placed at all and counts as recorded: a duplicate
// cannot be taken back out of an append-only log, a gap can still be explained.
//
// A log that cannot be read is treated the same way, and logged.
func (l *Loop) completedRoundExitEvent(ctx context.Context, completedAt int) lifecyclecore.Event {
	if l.LifecycleMemory == nil {
		return idleYieldEventFor(false) // no signed log wired: nothing to backfill
	}
	// A full read, like claimAndRecover's: once per Run, off seqMu, and the
	// answer has to cover the whole log rather than a lookback-bounded tail.
	events, err := lifecycle.ReadOrdered(ctx, l.LifecycleMemory, l.lifecycleScope())
	if err != nil {
		slog.Default().Info("resume park: read lifecycle log failed; recording the idle exit without checking for the round's AgentWorkComplete",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return idleYieldEventFor(false)
	}
	for _, e := range lifecycle.ForIncarnation(events, l.sessionIncarnationUID()) {
		if _, isCompletion := e.Event.(lifecyclecore.AgentWorkComplete); !isCompletion {
			continue
		}
		if e.Key.Region == "" || channelevents.UnpackTurn(e.Key.Seq) >= completedAt {
			return idleYieldEventFor(false)
		}
	}
	slog.Default().Info("resume park: the completed round has no AgentWorkComplete in the lifecycle log; recording it on this exit",
		"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "completedAt", completedAt)
	return lifecyclecore.AgentWorkComplete{Kubectl: false}
}

// parkUntilInbound blocks in the SAME wait the agent's own await_user_message
// uses, returning true when new input arrived and false when the runner should
// exit (idle TTL elapsed, or the context was cancelled).
//
// It borrows the registered tool rather than reimplementing the wait, because
// that wait is where three separate rules already meet: the idle TTL, the
// presence heartbeat that extends it while a dashboard is being watched, and the
// OnYield/OnResume hooks that persist accrued run-duration across a sleep. A
// second wait beside it would re-derive all three and drift from them silently —
// the runner would sleep by two different sets of rules depending on why it woke.
//
// A session with no such tool (kubectl-driven, not channel-attached) cannot park
// at all: nothing can deliver it a message. It reports false and the caller
// exits — correct, because such a session that has already answered its prompt
// is genuinely finished.
func (l *Loop) parkUntilInbound(ctx context.Context, sess *tool.SessionContext) bool {
	await, ok := l.lookupTool(awaitUserMessageToolName)
	if !ok {
		slog.Default().Info("resume with an empty inbox and no await tool: this session cannot receive a message, so it is finished",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name)
		return false
	}

	res, err := await.Execute(ctx, json.RawMessage(`{}`), sess)
	if err != nil {
		// Never silently proceed into a turn: the wait is what stands between
		// this runner and an invalid request, so a wait that failed means exit,
		// not carry on. Logged because an operator seeing a session end here
		// otherwise has nothing to distinguish it from an ordinary idle exit.
		slog.Default().Info("resume park: await failed; exiting rather than running a turn with nothing to answer",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "err", err.Error())
		return false
	}
	if res.IsError {
		slog.Default().Info("resume park: await reported an error; exiting",
			"session", l.SessionKey.Namespace+"/"+l.SessionKey.Name, "content", res.Content)
		return false
	}
	// The await tool reports a TTL expiry by asking the loop to stop rather
	// than by erroring — the same signal the ordinary path acts on.
	if res.Terminal {
		return false
	}
	return true
}
