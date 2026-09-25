// pkg/agent/runner/completion_gate.go
//
// The settle gate: a declared completion requirement is checked wherever a
// session comes to rest, not only where it asks to.
package runner

import (
	"context"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/completion"
	"github.com/authzed/openagentprimitives/pkg/agent/llm"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/x/besteffort"
)

// completionRequirements are the keys this session's class declared. Read from
// the live AgentClass pointer rather than mirrored onto a Loop field, so the
// gate and the terminal tool the capability layer wires — whichever one this
// session got — cannot come to disagree about what this class promised. Both
// terminal tools are built from the same CompletionConfig for that reason
// (pkg/agent/tool/meta/capability's terminalToolFor). Nil class ⇒ no requirements,
// which leaves the gate inert for every kubectl-driven session and test fixture
// that carries no class.
func (l *Loop) completionRequirements() []string {
	if l.AgentClass == nil {
		return nil
	}
	return l.AgentClass.Spec.CompletionRequirements
}

// completionSettleRefusal evaluates the declared requirements at a settle
// boundary and returns the model-facing refusal when any is unmet.
//
// # Why the runner and not each exit tool
//
// completion.Evaluate ran only inside the terminal tool, so a requirement was
// checked only on the one door that asks permission — and a delegated child
// leaves through a different one of those (return_result), which is why that
// tool shares the same gate rather than carrying its own. A session leaves
// through several more: a Terminal+IdleExit tool result, and a turn
// carrying no tool_use at all, which the loop recovers to Idle. That last one
// is why this cannot be a per-tool check at any price: a model that calls
// nothing has made no call to intercept, and a live run ended exactly that way
// with the requirement never evaluated. Enforcing where the session actually
// comes to rest routes every exit — including ones not yet written — through
// one predicate.
//
// # Refusing is not the same as refusing to park
//
// An agent that parks to ask a clarifying question before it has produced
// anything owes nothing, and each Requirement says so for itself: the
// artifact-delivered kind compares what was RENDERED against what was
// DELIVERED, so a session that rendered nothing reports Met. The gate refuses a
// settle with something OUTSTANDING, never a park.
//
// # Fail-closed, and bypassable
//
// An Evaluate error is folded in as one more unmet entry rather than waved
// through, exactly as the terminal tool does with it: a requirement that
// cannot answer must not read as satisfied. Both that fault and a genuine gap
// stay escapable, because a requirement with no way out turns a degraded round
// into a wedged session. The escape is the terminal tool's `bypass_reason`,
// which is the one surface that can carry a reason and record it for a human —
// so the refusal names it rather than inventing a second one.
func (l *Loop) completionSettleRefusal(ctx context.Context, sess *tool.SessionContext) (string, bool) {
	keys := l.completionRequirements()
	if len(keys) == 0 {
		return "", false
	}
	unmet, err := completion.Evaluate(ctx, keys, completion.Input{Session: sess})
	if err != nil {
		unmet = append(unmet, completion.CheckFailedUnmet(err))
	}
	if len(unmet) == 0 {
		return "", false
	}
	return settleRefusalMessage(l.terminalToolName(), unmet), true
}

// terminalToolName names the tool THIS session finishes through, so a refusal
// tells the model to call something it actually has.
//
// A delegated child is offered return_result IN PLACE OF agent_work_complete
// (pkg/agent/tool/meta/capability/core.go), so naming the absent one would hand
// it an escape it cannot reach — the wedge the bypass exists to prevent.
// Answered by asking the session's own tool table rather than by re-deriving
// the swap rule from spec.parent, because a second copy of that rule is a
// second thing to keep in step: the tools are the ground truth about what this
// session can call.
func (l *Loop) terminalToolName() string {
	if _, ok := l.lookupTool("return_result"); ok {
		return "return_result"
	}
	return "agent_work_complete"
}

// settleRefusalMessage renders the refusal the model reads at a settle it was
// not allowed to make.
//
// It leads with what happened rather than with the list, because the model did
// not ask to be refused here — unlike the terminal tool's own gate, it reached
// this by ending a turn — and has to be told the round is still open before the
// detail means anything. The bypass is spelled out in full and last: an agent
// that genuinely cannot satisfy a requirement must be able to leave, and the
// only failure worse than settling with a debt is looping forever against one.
func settleRefusalMessage(terminalTool string, unmet []completion.Unmet) string {
	var b strings.Builder
	b.WriteString("BLOCKED: this round is NOT over and the session has NOT ended. " +
		"You ended your turn without calling " + terminalTool + ", but this agent has completion " +
		"requirements and some are still unmet:\n")
	b.WriteString(completion.UnmetList(unmet))
	b.WriteString("\nDo what each one names above, then call " + terminalTool + " to finish. " +
		"If you genuinely cannot — a render that never came back, an outage, a refusal — call " +
		terminalTool + " with a non-empty `bypass_reason` saying why, in the user's own terms. " +
		"That ends the round and the reason is shown to them. Do NOT simply end your turn again: " +
		"the round cannot close that way and you will be told this same thing.")
	return b.String()
}

// refusalTurn wraps a settle refusal as the user-role message the loop splices
// in where there is no tool_result to carry it — a turn that called no tool,
// and a resume whose park expired before any turn ran.
//
// Deliberately NOT appended to the durable transcript. It is a runtime
// correction, not something anyone said, and persisting it as a "user" turn
// would put words in the user's mouth for every later replay. Losing it on
// restart costs nothing: the requirement is re-evaluated at the next settle,
// and if it is still unmet the same refusal is produced again.
func refusalTurn(refusal string) llm.Message {
	return llm.Message{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: refusal}}}
}

// noteCompletionSettleBlocked records a blocked settle where an operator can
// find it. exit names the door the session tried to leave by, so a session that
// looks stuck at Running is explicable from `kubectl describe agentsession`
// without reading the runner's logs.
func (l *Loop) noteCompletionSettleBlocked(ctx context.Context, exit string) {
	ref := l.SessionKey.Namespace + "/" + l.SessionKey.Name
	slog.Default().Info("completion gate: blocked a settle with unmet requirements",
		"session", ref, "exit", exit)
	// Status is a *StatusPatcher whose methods dereference it, so the nil check
	// is load-bearing rather than defensive: dispatch-level tests construct a
	// Loop without one and a blocked settle must not panic them.
	if l.Status != nil {
		besteffort.Log(slog.Default().Info, "AppendRunnerNote (completion gate)",
			l.Status.AppendRunnerNote(ctx,
				"completion gate: blocked settle via "+exit+" — a declared completion requirement is unmet"),
			"session", ref)
	}
}
