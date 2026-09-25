package meta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// maxQuestionRunes mirrors ParentExchange.Question's own
// +kubebuilder:validation:MaxLength=4096, enforced here too so Execute never
// attempts a status write the apiserver would reject. Refused rather than
// truncated, for the same reason delegate refuses an over-long task: a
// truncated question can silently become a different question, and the parent
// answering it has no way to know it read half of one.
const maxQuestionRunes = 4096

// ErrExchangeBudgetSpent is returned by AskParentConfig.Record when this
// delegation has no clarification budget left — a `task` child asking a second
// question, per SubagentRequestSpec.Mode.
//
// A SENTINEL so this tool can tell it apart from a failed write. The two need
// completely different words: a failed write means the question was never
// recorded and the child should decide for itself, while this one means the
// question was recorded by nobody ON PURPOSE and asking again in any form will
// be refused too. Both end the call without parking, which is the point —
// neither leaves the child waiting on an answer that is not coming.
//
// The ceiling itself is NOT read here or by whatever supplies Record. It is
// resolved once by the SubagentRequest controller, which counts it down on the
// request's status; Record only observes that count. The controller refuses the
// exchange regardless of what a runner does with this, so a runner that skipped
// the check gains an unanswerable park, not an extra question.
var ErrExchangeBudgetSpent = errors.New("ask_parent: the delegation's clarification budget is spent")

// AskParentConfig wires ask_parent. Record is the status write; the embedded
// Await is the SAME configuration await_user_message gets, because the park
// this tool performs is that park — see yieldAndWait for why the two must not
// have separate wait loops.
type AskParentConfig struct {
	// Record persists the question on this session's own
	// status.parentExchange and returns the exchange number it landed as.
	// Required; a nil Record refuses every call rather than panicking.
	//
	// It returns ErrExchangeBudgetSpent, and records nothing, when the
	// delegation has already spent the questions its mode allows.
	Record func(ctx context.Context, question string) (int64, error)

	// Await is the yield configuration: idle TTL, the inbound channel the
	// parent's reply arrives on, and the yield/resume hooks that stop the run
	// clock, disarm the silence watchdog and flush accrued run-time.
	Await AwaitConfig
}

// NewAskParent constructs the ask_parent meta tool.
func NewAskParent(cfg AskParentConfig) tool.Tool { return &askParentTool{cfg: cfg} }

type askParentTool struct{ cfg AskParentConfig }

func (*askParentTool) Name() string    { return "ask_parent" }
func (*askParentTool) Kind() tool.Kind { return tool.KindMeta }
func (*askParentTool) Permission() authz.Permission {
	// ask_parent writes one field of this session's own status and then
	// blocks, exactly as await_user_message blocks. It reads and mutates no
	// external resource; whether this session may converse with its parent at
	// all was decided when the delegation was authorized, and is re-checked
	// on the parent's reply by the agentsession#converse gate on the inbound
	// path.
	return authz.Permission{StateImpact: authz.Stateless}
}
func (*askParentTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*askParentTool) Description() string {
	return "Ask the agent that delegated this task to you a question, and wait for its answer. Use this when you " +
		"genuinely cannot proceed without something only the delegating agent knows — a missing constraint, an " +
		"ambiguity in the task, a choice it has to make. It is NOT for progress updates: nobody reads those, and each " +
		"question costs the other agent a turn. There is no human on the other end. Ask ONE self-contained question; " +
		"the answer arrives as the next message in this conversation, and you continue from there. If the delegating " +
		"agent never answers, the delegation is ended for you rather than left waiting forever, so do not ask what you " +
		"could reasonably decide yourself."
}

func (*askParentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"question": {
				"type": "string",
				"description": "One self-contained question for the agent that delegated this task. It sees only these words -- not your reasoning, your tool results, or anything else in this session -- so include whatever context the question needs to be answerable."
			}
		},
		"required": ["question"]
	}`)
}

type askParentArgs struct {
	Question string `json:"question"`
}

func (t *askParentTool) Execute(ctx context.Context, raw json.RawMessage, _ *tool.SessionContext) (tool.Result, error) {
	var args askParentArgs
	if res, ok := tool.ParseArgs(raw, &args, t.Name(), `{"question": "which of the two repos should I open the PR against?"}`); !ok {
		return res, nil
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return tool.Result{Content: fmt.Sprintf("%s: question must not be empty", t.Name()), IsError: true, Trusted: true}, nil
	}
	if len([]rune(question)) > maxQuestionRunes {
		return tool.Result{
			Content: fmt.Sprintf("%s: question is too long (%d runes, max %d) -- shorten it rather than relying on truncation, which would silently change what you asked",
				t.Name(), len([]rune(question)), maxQuestionRunes),
			IsError: true, Trusted: true,
		}, nil
	}
	if t.cfg.Record == nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: not available in this session (nothing wired to carry a question to a delegating agent)", t.Name()),
			IsError: true, Trusted: true,
		}, nil
	}

	// Record BEFORE yielding. The other order would park the session on a
	// question no one can see: the parent learns of it only through this
	// write, so a yield that preceded a failed write would wait out the whole
	// idle TTL in silence and then exit as though nothing had been asked.
	exchange, err := t.cfg.Record(ctx, question)
	if errors.Is(err, ErrExchangeBudgetSpent) {
		// A REFUSAL, not a failure, and it deliberately ends the call here:
		// the session is not parked, nothing is torn down, and the model has
		// the rest of this turn to act on it. Say what was spent and what to do
		// instead, because "budget exhausted" alone invites a retry, and every
		// retry is refused the same way.
		return tool.Result{
			Content: fmt.Sprintf("%s: this delegation's clarification budget is spent — you have already asked the agent that delegated this task everything it will carry, so this question was NOT recorded and no answer is coming. Do not ask again. Proceed with what you already have, or finish now and report what you could not resolve. (%v)",
				t.Name(), err),
			IsError: true, Trusted: true,
		}, nil
	}
	if err != nil {
		return tool.Result{
			Content: fmt.Sprintf("%s: recording the question failed, so the delegating agent will never see it: %v. Do not wait for an answer; carry on, or finish and report what you could not resolve.",
				t.Name(), err),
			IsError: true, Trusted: true,
		}, nil
	}

	switch yieldAndWait(ctx, t.cfg.Await) {
	case awaitDisabled:
		// No idle TTL means there is no wait to perform: the session yields to
		// Idle immediately and the pod exits. The question STANDS — it is
		// recorded and pending, and the parent's reply re-hydrates this
		// session — so this is a park, not a failure, and the text says so.
		return tool.Result{
			Content:  "question recorded; parking until the delegating agent answers (this session exits to phase=Idle and resumes on the reply)",
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	case awaitResumed:
		// Same bare acknowledgment await_user_message returns, and for the
		// same reason: Loop.drainInbox has already spliced the parent's reply
		// in as the next turn, so pointing the model at memory would make a
		// missed drain catastrophic instead of merely quiet.
		return tool.Result{
			Content:      "acknowledged",
			Terminal:     false,
			AwaitResumed: true,
			Trusted:      true,
		}, nil
	case awaitTTL:
		// The pod exits to Idle and the question stays pending, so the reply
		// still wakes this session later. What bounds it is the SubagentRequest
		// controller's parent-reply timeout, not this TTL.
		slog.Info("ask_parent: idle TTL expired while awaiting the delegating agent; parking",
			"exchange", exchange)
		return tool.Result{
			Content:  "no answer yet; parking until the delegating agent replies (this session exits to phase=Idle and resumes on the reply)",
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	default: // awaitCanceled
		return tool.Result{
			Content:  "session canceled (SIGTERM) while awaiting the delegating agent; exiting cleanly to phase=Idle",
			Terminal: true, IdleExit: true,
			Trusted: true,
		}, nil
	}
}
