package meta

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// return_result is a DELEGATED CHILD's terminal tool, offered in place of
// agent_work_complete rather than alongside it.
//
// The two are not the same act wearing different words. agent_work_complete
// ends a round for a session with a human reader: what that person sees went
// out earlier through respond_to_user, and its `summary` is an audit note that
// only ever reaches `kubectl describe`. A delegated child has neither. It has
// no human, and respond_to_user is withheld from it — for such a session that
// tool would reach the delegating agent's transcript with no content
// inspection anywhere.
//
// What crosses back instead is exactly one string: subagentrequest's
// reconcileChild copies child.Status.Result.Summary verbatim into
// SubagentRequest.Status.Result, which delegate() hands to the caller as its
// tool result. So for a child that field is not a note ABOUT the work — it IS
// the work.
//
// This existed as a conditional description on agent_work_complete for one
// commit, and that was the wrong shape: a tool's NAME is the strongest prompt
// it has, and "mark this round of work complete" frames the payload as
// incidental no matter what the description underneath says. Observed live: a
// child called it, wrote "translated the line and replied to user", and its
// parent — receiving a status report where the translation should have been —
// delegated again, and again, nine turns deep.
//
// What it does NOT get to skip is the completion gate. A class's completion
// requirements are the operator's guarantee about what a session of that class
// produces, and a child reporting to an agent rather than to a person does not
// void that promise — it is exactly the session nobody is watching. So this
// runs the SAME completionGate agent_work_complete runs, bypass path included,
// from the same config.
//
// Terminality is signalled by Result.Terminal, which the runner keys on rather
// than on any tool name, so this yields the session exactly as
// agent_work_complete does and emits the same lifecycle event. The session
// ending is the same fact either way; only who reads the payload differs.
type returnResult struct{ gate completionGate }

// NewReturnResult returns the delegated-child terminal tool carrying cfg — the
// same per-session completion gate agent_work_complete is constructed with, so
// the two doors out of a session cannot come to disagree about what the class
// promised.
func NewReturnResult(cfg CompletionConfig) tool.Tool {
	return &returnResult{gate: completionGate{cfg: cfg}}
}

func (*returnResult) Name() string    { return "return_result" }
func (*returnResult) Kind() tool.Kind { return tool.KindMeta }

// Permission mirrors agent_work_complete's Passthrough, and for the same
// reason: ending your own session needs no grant beyond the
// AgentSession#interact check already gating the session, but a recorded
// completion bypass reaches a reader as a notice on the session's channel,
// which is observable state. Neither value requires a Check
// (StateImpact.CheckRequired is false for both), so nothing about dispatch
// changes; what changes is that the declared impact stops understating what
// the call can do.
func (*returnResult) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

func (*returnResult) PermissionVariants() []authz.PermissionVariant { return nil }

func (r *returnResult) Description() string {
	return "Finish the work you were delegated and return your answer to the agent that delegated it. `result` IS that answer: it is passed to your caller verbatim and is the only thing it receives. Put the actual content there — the translated text, the analysis, the list — never a description of what you did, because \"translated the line\" tells your caller nothing and it cannot ask you again. There is no human in this session and no respond_to_user to post to. If you cannot finish without something from your caller, use ask_parent instead of returning a partial answer. Call this exactly once, when you have the answer." +
		r.gate.descriptionAddendum()
}

func (r *returnResult) InputSchema() json.RawMessage {
	props := map[string]any{
		"result": map[string]any{
			"type":        "string",
			"description": "Your answer, delivered verbatim to the agent that delegated this work. The deliverable itself, not a report about it — your caller receives exactly this string and nothing else.",
		},
		"artifacts": map[string]any{
			"type": "array",
			"description": "Files you rendered that your caller should have. Handing one over is delivery to your CALLER, not to a user — " +
				"nobody sees it until your caller chooses to attach it to a reply of its own.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					// The FORM is load-bearing, so it is spelled out: the
					// caller attaches this handle by asking the API server for
					// the render it names, which only the `ar-…` handle does.
					// An artifact_id or a tagged revision resolves through THIS
					// session's own store, which the caller cannot read, and
					// would fail at the moment the caller tried to deliver it.
					"id":          map[string]any{"type": "string", "description": "The `handle` (the `ar-…` value) that artifact_prepare or artifact_await returned for this artifact. Not the artifact_id and not a tag — your caller can only attach the handle."},
					"description": map[string]any{"type": "string", "description": "What this artifact represents. Your caller sees this next to the handle and nothing else about the file, so name the content, not the act of making it."},
				},
				"required": []string{"id", "description"},
			},
		},
	}
	r.gate.addSchemaProperties(props)
	return marshalSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             []string{"result"},
	}, "result")
}

type returnResultArgs struct {
	Result    string                `json:"result"`
	Artifacts []tool.ResultArtifact `json:"artifacts"`
	// BypassReason is a POINTER for the same reason agent_work_complete's is:
	// "sent an empty string" and "did not send the field" are different acts
	// and get different refusals.
	BypassReason *string `json:"bypass_reason,omitempty"`
}

func (r *returnResult) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args returnResultArgs
	if res, ok := tool.ParseArgs(raw, &args, r.Name(), `{"result": "the answer itself", "artifacts": [...]} (artifacts optional)`); !ok {
		return res, nil
	}
	if args.Result == "" {
		return tool.Result{
			Content: "return_result: `result` is required and must be non-empty. It is the answer your caller receives, verbatim — an empty one tells it the work produced nothing. If you genuinely have nothing to return, say why in `result` rather than leaving it blank.",
			IsError: true, Trusted: true,
		}, nil
	}
	// A nil SessionContext or SubmitResult is a runner programming error, not
	// bad model input, so it returns a Go error (fatal) rather than IsError.
	if sess == nil || sess.SubmitResult == nil {
		return tool.Result{Trusted: true}, errors.New("return_result: SessionContext.SubmitResult is nil")
	}

	if res, ok := r.gate.check(ctx, r.Name(), args.BypassReason, sess); !ok {
		return res, nil
	}

	// Written into AgentResult.Summary because that is the field
	// reconcileChild reads back. The ARGUMENT is named `result` on purpose —
	// the status field's name is an internal detail, and calling it "summary"
	// to the model is what produced summaries instead of answers.
	sess.SubmitResult(tool.AgentResult{Summary: args.Result, Artifacts: args.Artifacts})
	return tool.Result{Content: "result returned to the delegating agent", Terminal: true, Trusted: true}, nil
}
