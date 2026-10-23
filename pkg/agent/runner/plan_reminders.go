package runner

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/session/state/plans"
	"github.com/authzed/openagentprimitives/pkg/agent/tool/meta"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/platform/pipeline"
)

// PreparePlanReminders asks the operator to prepare exact, signed consent cards.
// Preparation grants no execution authority. The frozen cards are shown and
// decided together with the phase's other authority, through channelsd.
func PreparePlanReminders(ctx context.Context, phases []plans.Phase, call meta.GoalsCaller, allowed bool) ([]plangate.AuthoredPhase, error) {
	authored := AuthoredPhasesFrom(phases)
	seen := map[string]bool{}
	count := 0
	for _, phase := range phases {
		for _, reminder := range phase.Reminders {
			count++
			if !allowed || call == nil {
				return nil, fmt.Errorf("reminders are unavailable in bounded or delegated sessions")
			}
			if count > 8 || reminder.ID == "" || seen[reminder.ID] {
				return nil, fmt.Errorf("plan reminders require distinct goals (maximum 8)")
			}
			seen[reminder.ID] = true
		}
	}
	for i, phase := range phases {
		for _, reminder := range phase.Reminders {
			req := reminder.ExecutionRequest
			req.ApprovalMode = "plan"
			response, err := call(ctx, goals.Request{Operation: "request_execution", Resource: reminder.Resource, Execution: req})
			if err != nil {
				return nil, fmt.Errorf("prepare plan reminder: %w", err)
			}
			if response.Approval == nil {
				return nil, fmt.Errorf("plan reminder approval unavailable")
			}
			raw, err := json.Marshal(response.Approval)
			if err != nil {
				return nil, err
			}
			authored[i].Consents = append(authored[i].Consents, raw)
		}
	}
	return authored, nil
}

// RequestPlanReminderApproval presents prepared reminders immediately, rather
// than requiring an unrelated permissioned call to happen in an empty phase.
func (l *Loop) RequestPlanReminderApproval(ctx context.Context) error {
	plan, declared := l.ActiveFrozenPlan(ctx)
	if !declared {
		return nil
	}
	for i, phase := range plan.Phases {
		if len(phase.Consents) == 0 {
			continue
		}
		l.executor()
		if l.pipelineReg == nil {
			return fmt.Errorf("plan consent gate unavailable")
		}
		var decision pipeline.Decision
		found := false
		for _, hook := range l.pipelineReg.Hooks(pipeline.PreToolCall) {
			requester, ok := hook.(interface {
				ConsentApproval(context.Context, pipeline.SessionRef, int) (pipeline.Decision, error)
			})
			if !ok {
				continue
			}
			var err error
			decision, err = requester.ConsentApproval(ctx, pipeline.SessionRef{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name}, i)
			if err != nil {
				return err
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("plan consent gate unavailable")
		}
		if decision.Approval == nil {
			continue
		}
		host := newRunnerHost(l, hostSession{Namespace: l.SessionKey.Namespace, Name: l.SessionKey.Name, Class: l.AgentName})
		id, err := host.PublishApproval(ctx, *decision.Approval)
		if err != nil {
			return err
		}
		approved, _, timedOut, err := host.AwaitDecision(ctx, id, l.resolvedApprovalTimeout())
		if err != nil {
			return err
		}
		if !approved || timedOut {
			return fmt.Errorf("plan reminder approval was denied or expired")
		}
		st, err := plangate.Fold(plan, l.PlanGateRecords())
		if err != nil || !st.PhaseApproved(i) {
			return fmt.Errorf("plan reminder approval could not be recorded")
		}
	}
	return nil
}
