package goals

import (
	"context"
	"fmt"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
)

// derivePlanApproval runs after the normal live goal-session authority checks.
// It reads the frozen plan from durable memory, rather than accepting a caller's
// ceiling or a claimed approval. The runner records the returned authority in
// its signed plan-gate log before the gate can use it.
func (s *Server) derivePlanApproval(ctx context.Context, sess *v1.AgentSession, request *domain.PlanApprovalRequest) (*plangateaudit.ApprovalAuthority, error) {
	if request == nil || request.Digest == "" || request.Phase < 0 {
		return nil, domain.ErrInvalid
	}
	store, ok := s.Service.Store.(domain.OccurrenceStore)
	if !ok {
		return nil, domain.ErrDenied
	}
	o, err := store.Occurrence(ctx, sess.Spec.GoalExecution.OccurrenceID)
	if err != nil {
		return nil, err
	}
	g, err := s.Service.Store.Get(ctx, o.Domain, o.GoalID)
	if err != nil {
		return nil, err
	}
	if err := s.Service.DispatchOccurrence(ctx, g, o); err != nil {
		return nil, err
	}
	if g.Execution.Terms.ActionApproval != "standing_private" {
		return nil, nil // Existing/default consent still requires a human.
	}
	records, err := plangateaudit.List(ctx, s.Memory, memory.Scope{Kind: "session", ID: sess.Namespace + "/" + sess.Name})
	if err != nil {
		return nil, err
	}
	latest := ""
	for _, rec := range records {
		if rec.Event == plangateaudit.EventPlanApproved {
			latest = rec.PlanDigest
		}
	}
	if latest != request.Digest {
		return nil, fmt.Errorf("%w: plan is no longer current", domain.ErrDenied)
	}
	plan, ok := plangate.PlanForDigest(records, latest)
	if !ok || plan.Digest() != latest {
		return nil, fmt.Errorf("%w: frozen plan cannot be reconstructed", domain.ErrDenied)
	}
	state, err := plangate.Fold(plan, records)
	if err != nil || state.Doubtful || state.ActivePhase != request.Phase || request.Phase >= len(plan.Phases) {
		return nil, fmt.Errorf("%w: phase state cannot be established", domain.ErrDenied)
	}
	if state.IsDenied(plangate.PhaseRef{PlanDigest: latest, Index: request.Phase}) || state.BudgetExhausted(request.Phase) {
		return nil, domain.ErrDenied
	}
	// Check the WHOLE plan. A fresh plan which asks for unrelated authority
	// does not silently gain approval for one of its private-reporting phases.
	for _, phase := range plan.Phases {
		if len(phase.Slots) != 0 || len(phase.Consents) != 0 {
			return nil, fmt.Errorf("%w: standing private delivery cannot grant resources or new consents", domain.ErrDenied)
		}
		for _, handle := range phase.Permissions {
			if handle.String() != "perm:execute:agent_goal_execution" {
				return nil, fmt.Errorf("%w: plan exceeds standing private delivery authority", domain.ErrDenied)
			}
		}
	}
	return &plangateaudit.ApprovalAuthority{Kind: "goal_private_delivery", Reference: g.Execution.Digest,
		DecisionRef: g.Execution.Decision.RequestID, OccurrenceID: o.ID, SessionUID: string(sess.UID)}, nil
}
