package runner

import (
	"context"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// GoalPlanApprovalDeriver asks the trusted operator to derive each fresh plan's
// approval. It is wired only for bounded goal roots. Ordinary sessions retain
// their existing approval path, and a nil reply means a human must approve.
func GoalPlanApprovalDeriver(call meta.GoalsCaller) func(context.Context, plangate.Plan, int) (*plangateaudit.ApprovalAuthority, error) {
	return func(ctx context.Context, plan plangate.Plan, phase int) (*plangateaudit.ApprovalAuthority, error) {
		if call == nil {
			return nil, fmt.Errorf("goal plan authority unavailable")
		}
		response, err := call(ctx, goals.Request{Operation: "authorize_plan", PlanApproval: &goals.PlanApprovalRequest{Digest: plan.Digest(), Phase: phase}})
		return response.PlanApproval, err
	}
}
