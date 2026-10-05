package meta

import (
	"context"
	"encoding/json"

	"github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// BoundedGoalTools exposes one reviewed report action and local plan controls.
// The report carries an explicit permission so the normal enforcing plan gate
// requires a fresh approved phase. The authority callback rechecks live consent
// immediately before publication, including account suspension and revocation.
func BoundedGoalTools(tools []tool.Tool, digest string, authorize func(context.Context) error) []tool.Tool {
	var bounded []tool.Tool
	for _, candidate := range tools {
		switch candidate.Name() {
		case "respond_to_user":
			bounded = append(bounded, &goalReport{Tool: candidate, digest: digest, authorize: authorize})
		case "update_plan", "select_phase", "complete_phase", "agent_work_complete":
			bounded = append(bounded, candidate)
		}
	}
	return bounded
}

type goalReport struct {
	tool.Tool
	digest    string
	authorize func(context.Context) error
}

func (*goalReport) PipelineRouted() bool { return true }
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
