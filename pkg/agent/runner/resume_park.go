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
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// awaitUserMessageToolName is the meta tool the agent calls to park. The loop
// borrows it by name for the resume case; see parkUntilInbound.
const awaitUserMessageToolName = "await_user_message"

// resumedWithNothingToDo reports whether this Run has nothing to respond to.
//
// Both halves are required and neither is sufficient:
//
//   - hadInitialPrompt distinguishes a RESUME from a first boot. A first boot
//     always has something to say — its opening prompt — and must never park.
//   - The last replayed message being the ASSISTANT's is what makes the turn
//     both pointless and invalid. If the transcript ends with a user message (a
//     just-drained inbox turn, or a prior run that died mid-turn before
//     answering), there IS something outstanding and the loop must run.
//
// It checks the message shape rather than "did drainInbox return anything"
// because the question is what the model would be sent, which the messages
// themselves answer — so a future path that appends something else for the model
// to act on is covered without knowing about it.
func resumedWithNothingToDo(hadInitialPrompt bool, messages []llm.Message) bool {
	if !hadInitialPrompt || len(messages) == 0 {
		return false
	}
	return messages[len(messages)-1].Role == "assistant"
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
