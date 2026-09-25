package meta

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

func init() {
	// Register the no-config default (kubectl-driven sessions).
	// Sessions whose AgentClass declares completion requirements construct via
	// NewAgentWorkComplete(cfg) and override the registered tool in the
	// runner's tool list.
	Register(&agentWorkComplete{})
}

// NewAgentWorkComplete returns an agent_work_complete tool carrying cfg.
// Callers keep this constructor (vs the registry-registered default) so the
// completion gate can be wired per session. return_result — the delegated
// child's terminal tool — takes the same config and runs the same gate.
func NewAgentWorkComplete(cfg CompletionConfig) tool.Tool {
	return &agentWorkComplete{gate: completionGate{cfg: cfg}}
}

type agentWorkComplete struct{ gate completionGate }

func (*agentWorkComplete) Name() string    { return "agent_work_complete" }
func (*agentWorkComplete) Kind() tool.Kind { return tool.KindMeta }

// Permission is Passthrough.
//
// It was Stateless — "touches nothing observable" — while the tool did nothing
// but end the loop and write a summary to status. The completion gate changed
// that: a recorded bypass reaches a person as a notice on the session's
// channel, which is observable state in exactly the sense respond_to_user's
// Passthrough covers, and touches no SpiceDB resource. Neither value requires a
// Check (StateImpact.CheckRequired is false for both), so nothing about
// dispatch changes; what changes is that the declared impact stops
// understating what the call can do.
func (*agentWorkComplete) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Passthrough}
}

// PermissionVariants returns nil — meta tools have no conditional
// variants today (only MCP-tooled AgentClasses use them).
func (*agentWorkComplete) PermissionVariants() []authz.PermissionVariant { return nil }

func (a *agentWorkComplete) Description() string {
	return "Mark this round of work complete. The summary is recorded on the AgentSession status (visible via `kubectl describe`) for audit/debug, but it is NOT posted to the channel. If you want the user to see something, send it via respond_to_user FIRST, then call agent_work_complete to end the round. The session goes idle (the conversation persists, awaiting any follow-up); a follow-up message in the same thread resumes it. Call this exactly once when you are done with the user's current request." +
		a.gate.descriptionAddendum()
}

func (a *agentWorkComplete) InputSchema() json.RawMessage {
	props := map[string]any{
		"summary": map[string]any{
			"type":        "string",
			"description": "One-line audit description of what you did this round. Recorded on AgentSession.status for kubectl/debug only — it is NOT posted to the channel and the user never reads it. Anything the user must actually read has to go out in a respond_to_user call BEFORE this one.",
		},
		"artifacts": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"id":          map[string]any{"type": "string", "description": "ArtifactStore ref returned by an earlier tool call."},
					"description": map[string]any{"type": "string", "description": "What this artifact represents."},
				},
				"required": []string{"id", "description"},
			},
		},
	}
	a.gate.addSchemaProperties(props)
	return marshalSchema(map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           props,
		"required":             []string{"summary"},
	}, "summary")
}

type completeArgs struct {
	Summary   string                `json:"summary"`
	Artifacts []tool.ResultArtifact `json:"artifacts,omitempty"`
	// BypassReason is a POINTER so "sent an empty string" stays distinguishable
	// from "did not send the field". They get different refusals: the first is
	// an attempted bypass with nothing said, the second is an ordinary
	// completion that has not been told about the gate yet.
	BypassReason *string `json:"bypass_reason,omitempty"`
}

func (a *agentWorkComplete) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	var args completeArgs
	if res, ok := tool.ParseArgs(raw, &args, a.Name(), `{"summary": "final answer / report body", "artifacts": [...]} (artifacts optional)`); !ok {
		return res, nil
	}
	if args.Summary == "" {
		return tool.Result{Content: "agent_work_complete: `summary` is required and must be non-empty. The summary is for the audit trail (visible via kubectl), not for the user — what the user sees comes from respond_to_user. Write a one-sentence audit description of what you did this round.", IsError: true, Trusted: true}, nil
	}
	// A nil SessionContext or SubmitResult is a runner programming error
	// — not user-input invalid — so it returns a Go error rather than
	// IsError=true. The runner treats Go errors as fatal; IsError=true
	// is fed back to the model as a tool_result it can recover from.
	if sess == nil || sess.SubmitResult == nil {
		return tool.Result{Trusted: true}, errors.New("agent_work_complete: SessionContext.SubmitResult is nil")
	}

	if res, ok := a.gate.check(ctx, a.Name(), args.BypassReason, sess); !ok {
		return res, nil
	}

	// The summary is NOT posted to the channel. User-visible content
	// goes through respond_to_user; agent_work_complete is purely the
	// "I'm done with this round" signal. The summary lives on
	// AgentSession.status for kubectl introspection.
	sess.SubmitResult(tool.AgentResult{Summary: args.Summary, Artifacts: args.Artifacts})
	return tool.Result{Content: "session complete", Terminal: true, Trusted: true}, nil
}
