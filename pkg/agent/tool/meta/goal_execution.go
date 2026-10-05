package meta

import (
	"context"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// BoundedGoalTools exposes one reviewed report action and local plan controls.
// The report carries an explicit permission so the normal enforcing plan gate
// requires a fresh approved phase. The authority callback rechecks live consent
// immediately before publication, including account suspension and revocation.
func BoundedGoalTools(tools []tool.Tool, digest string, authorize func(context.Context) error, resultCaller ...GoalsCaller) []tool.Tool {
	var bounded []tool.Tool
	for _, candidate := range tools {
		switch candidate.Name() {
		case "respond_to_user":
			bounded = append(bounded, &goalReport{Tool: candidate, digest: digest, authorize: authorize})
		case "update_plan", "select_phase", "complete_phase", "agent_work_complete":
			bounded = append(bounded, candidate)
		}
	}
	if len(resultCaller) == 1 && resultCaller[0] != nil {
		bounded = append(bounded, &goalReport{Tool: &goalResultTool{call: resultCaller[0]}, digest: digest, authorize: authorize})
	}
	return bounded
}

type goalReport struct {
	tool.Tool
	digest    string
	authorize func(context.Context) error
}

func (*goalReport) PipelineRouted() bool { return true }
func (t *goalReport) Description() string {
	return t.Tool.Description() + " In this bounded goal session, both respond_to_user and report_goal_result require perm:execute:agent_goal_execution. Declare that permission in the fresh delivery phase before requesting approval, and cover both delivery and result reporting in that phase. These actions change state; do not declare an empty or readonly permission ceiling for them."
}
func (t *goalReport) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Readwrite, Check: &authz.PermissionCheck{ResourceType: "agent_goal_execution", Permission: "execute", ResourceIDExpr: "'" + t.digest + "'"}}
}
func (t *goalReport) Execute(ctx context.Context, args json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if t.authorize == nil {
		return tool.Result{IsError: true, Content: "Goal execution authority is unavailable."}, nil
	}
	if err := t.authorize(ctx); err != nil {
		return tool.Result{IsError: true, Content: "Goal execution authority was refused: " + err.Error()}, nil
	}
	return t.Tool.Execute(ctx, args, sess)
}

type goalResultTool struct{ call GoalsCaller }

func (*goalResultTool) Name() string                                  { return "report_goal_result" }
func (*goalResultTool) Kind() tool.Kind                               { return tool.KindMeta }
func (*goalResultTool) PermissionVariants() []authz.PermissionVariant { return nil }
func (*goalResultTool) Permission() authz.Permission {
	return authz.Permission{StateImpact: authz.Readwrite}
}
func (*goalResultTool) Description() string {
	return "Record your result for this bounded goal session before agent_work_complete. Use reported_success, blocked, failed or unknown. Include a concise summary and evidence references (for example the reminder tool call). This records your account, not verified delivery, and never completes the durable goal. Reuse requestID for identical retries."
}
func (*goalResultTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["requestID","status","summary","evidence"],"properties":{"requestID":{"type":"string","minLength":1,"maxLength":128},"status":{"type":"string","enum":["reported_success","blocked","failed","unknown"]},"summary":{"type":"string","minLength":1,"maxLength":4000},"evidence":{"type":"array","maxItems":32,"items":{"type":"string","minLength":1,"maxLength":512}}}}`)
}
func (t *goalResultTool) Execute(ctx context.Context, raw json.RawMessage, sess *tool.SessionContext) (tool.Result, error) {
	if sess == nil || sess.IsDelegatedChild || t.call == nil {
		return tool.Result{IsError: true, Content: "Goal result reporting is unavailable."}, nil
	}
	var p goals.RunProposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	if err := p.Validate(); err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	_, err := t.call(ctx, goals.Request{Operation: "report_execution_result", Proposal: p})
	if err != nil {
		return tool.Result{IsError: true, Content: err.Error()}, nil
	}
	return tool.Result{Content: "Goal result proposal recorded. This is the agent's report, not verified delivery or durable goal completion.", Trusted: true}, nil
}
