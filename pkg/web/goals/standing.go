package goals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"sigs.k8s.io/controller-runtime/pkg/client"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/kinds/plangateaudit"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
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
	policy := g.Execution.Terms.ActionApproval
	if policy != "standing_private" && policy != "standing_plan" {
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
	if policy == "standing_plan" {
		return s.deriveGroupStanding(ctx, sess, g, o, plan, request.Phase)
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
	return &plangateaudit.ApprovalAuthority{Kind: plangateaudit.AuthorityGoalPrivateDelivery, Reference: g.Execution.Digest,
		DecisionRef: g.Execution.Decision.RequestID, OccurrenceID: o.ID, SessionUID: string(sess.UID)}, nil
}

// deriveGroupStanding is derivePlanApproval's standing_plan branch, run after
// the shared live checks (occurrence, revision, DispatchOccurrence, current
// plan, no recorded denial). It returns nil — a human card, never a denial —
// when the run plan widens the standing plan, or when the consent's approver
// can no longer speak for this run: no longer a manager of the goal, or no
// longer holding every permission the active phase's slots would delegate.
// The returned authority names that approver; the runner binds the phase's
// slots with them as delegator. An infrastructure failure is an error.
func (s *Server) deriveGroupStanding(ctx context.Context, sess *v1.AgentSession, g domain.Goal, o domain.Occurrence, plan plangate.Plan, phase int) (*plangateaudit.ApprovalAuthority, error) {
	c := g.Execution
	if !g.Domain.GroupOwned() || c.Terms.StandingPlan == nil || c.Decision == nil {
		return nil, fmt.Errorf("%w: standing_plan consent is not a group goal's standing plan", domain.ErrDenied)
	}
	if !plangate.CoveredByStanding(*c.Terms.StandingPlan, plan) {
		return nil, nil
	}
	approver := c.Decision.Owner
	// The same predicate a manager's click is held to: live manage on
	// agent_goal:<id> AND listed in g.Managers (or a class admin).
	// A run whose approver no longer qualifies asks a human instead; logged,
	// because "why did a standing run ask?" is otherwise unanswerable.
	cardInstead := func(why string) {
		slog.Info("goal standing plan: the approver can no longer speak for this run; it needs a manager's approval",
			"goal", g.ID, "occurrence", o.ID, "session", sess.Namespace+"/"+sess.Name, "phase", phase, "reason", why)
	}
	if err := s.AuthorizeGoalManager(ctx, domain.Actor{Domain: g.Domain, User: approver}, g); err != nil {
		if errors.Is(err, domain.ErrDenied) {
			cardInstead("no longer a manager of the goal")
			return nil, nil
		}
		return nil, err
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: g.Domain.Namespace, Name: g.Domain.Class}, &class); err != nil {
		return nil, err
	}
	who := identity.CanonicalFromTrusted(approver, "group goal standing-plan approver")
	held, err := s.approverHoldsSlots(ctx, &class, plan.Phases[phase].Slots, who)
	if err != nil {
		return nil, err
	}
	if !held {
		cardInstead("no longer holds every permission the phase's slots delegate")
		return nil, nil
	}
	return &plangateaudit.ApprovalAuthority{Kind: plangateaudit.AuthorityGoalGroupStanding, Approver: who.String(), Reference: c.Digest,
		DecisionRef: c.Decision.RequestID, OccurrenceID: o.ID, SessionUID: string(sess.UID)}, nil
}

// approverHoldsSlots is the server side of the check a human click runs
// (runner approverCanDelegateSlots): a slot type the class marks standing
// `required` must have EVERY permission it confers held by the approver on the
// named instance; any other type's authority is the approval itself. Unlike
// the click path, a partial hold is not narrowed to what is held — the run
// falls back to a card instead, so an automatic approval never binds less, or
// differently, than the standing plan a manager approved.
func (s *Server) approverHoldsSlots(ctx context.Context, class *v1.AgentClass, slots []plangate.Slot, who identity.CanonicalUserID) (bool, error) {
	standing, transforms := map[string]string{}, map[string][]string{}
	for _, rs := range class.Status.ResolvedSlots {
		standing[rs.ResourceType] = rs.Standing
		transforms[rs.ResourceType] = rs.ValueTransforms
	}
	sets, single := class.Spec.SlotPermissionSets(), class.Spec.SlotPermissions()
	for _, slot := range slots {
		perms := sets[slot.Type]
		if len(perms) == 0 && single[slot.Type] != "" {
			perms = []string{single[slot.Type]}
		}
		if len(perms) == 0 {
			return false, nil // nothing the class declares can be delegated
		}
		if standing[slot.Type] != v1.StandingRequired {
			continue
		}
		if slot.ID == "" {
			return false, nil // no instance to check standing on
		}
		id, err := authz.NewObjectID(slot.ID, transforms[slot.Type])
		if err != nil {
			return false, nil // the instance cannot be named, so it cannot be delegated
		}
		for _, perm := range perms {
			ok, err := s.Auth.CheckOnResource(ctx, slot.Type, id.String(), perm, who, true)
			if err != nil {
				return false, err
			}
			if !ok {
				return false, nil
			}
		}
	}
	return true, nil
}
