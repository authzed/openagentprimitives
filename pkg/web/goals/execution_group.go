package goals

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	v1 "github.com/authzed/openagentprimitives/pkg/apis/v1alpha1"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds/registry"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Execution rules for group-owned goals. They are deliberately separate from the private rules in
// Validate: the private path is shaped for tool-less reminders to one person,
// and each of its guards is kept, replaced or dropped here explicitly.
//
//	kept      class UID + spec digest; commit-pinned skills; no widened
//	          session access; enforcing info-leakage, tool-call and plan gates
//	          with RequirePlan and no auto-approve; class bounds
//	replaced  identityMode agent (not userPassthrough) via groupQualifies;
//	          tool runtimes allowed; the AgentIdentity pin replaces the owner
//	          catalog pin; the start gate is checked for the deciding manager;
//	          the origin Channel UID + member ref replace the origin session
//	          (which is never read, so session GC cannot strand a goal);
//	          AllowedOperations are not narrowed; the group destination
//	          replaces Recipient == owner
//	dropped   executionPreference(owner)

// prepareGroupExecution fills a group goal's routing, class and identity pins
// from authoritative objects, never from the model's arguments, and freezes
// the requested standing plan.
func (s *Server) prepareGroupExecution(ctx context.Context, a domain.Actor, r *domain.ExecutionRequest) error {
	ns, name, ok := strings.Cut(a.Session, "/")
	if !ok || ns != a.Domain.Namespace {
		return domain.ErrDenied
	}
	if r.Terms.Event != nil {
		return errGroupEvent
	}
	var sess v1.AgentSession
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &sess); err != nil {
		return err
	}
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: a.Domain.Class}, &class); err != nil {
		return err
	}
	if string(class.UID) != a.Domain.ClassUID || !class.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: execution class changed", domain.ErrDenied)
	}
	b, ch, err := s.groupConversation(ctx, &sess)
	if err != nil {
		return err
	}
	if err := s.groupQualifies(ctx, &class, ch); err != nil {
		return err
	}
	host, _ := registry.GroupGoalHostFor(ch.Spec.Kind) // groupQualifies established it
	if ref, ok := host.ConversationMemberRef(b); !ok || ref != a.Domain.Owner {
		return fmt.Errorf("%w: this conversation is not the group that owns this goal", domain.ErrDenied)
	}
	if r.Terms.Bounds == (domain.ExecutionBounds{}) {
		r.Terms.Bounds = executionPolicy(&class).DefaultBounds
	}
	hash, err := digest(class.Spec)
	if err != nil {
		return err
	}
	r.Terms.ClassDigest = hash
	r.Terms.SkillVersions = skillVersions(&class)
	r.Terms.OwnerCatalogUID = ""
	r.Terms.Destination = domain.PrivateDestination{}
	r.Terms.Group = &domain.GroupDestination{Channel: ch.Name, ChannelUID: string(ch.UID), MemberGroup: a.Domain.Owner}
	r.Terms.AgentIdentity, r.Terms.AgentIdentityUID = class.Spec.AgentIdentity, ""
	if class.Spec.AgentIdentity != "" {
		var ai v1.AgentIdentity
		if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: class.Spec.AgentIdentity}, &ai); err != nil {
			return fmt.Errorf("%w: AgentIdentity %q: %v", domain.ErrDenied, class.Spec.AgentIdentity, err)
		}
		if ai.UID == "" || !ai.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w: AgentIdentity %q is being deleted", domain.ErrDenied, class.Spec.AgentIdentity)
		}
		r.Terms.AgentIdentityUID = string(ai.UID)
	}
	// The standing plan arrives authored (the update_plan phase shape) and is
	// frozen here, as a runtime plan is; a caller never supplies the frozen
	// form, which would let it skip the freeze's checks.
	if r.Terms.StandingPlan != nil {
		return fmt.Errorf("%w: give the standing plan as terms.standingPlan phases, in the shape update_plan takes", domain.ErrInvalid)
	}
	if len(r.StandingPlan) > 0 {
		frozen, err := freezeAuthoredStandingPlan(authoredStandingPhases(r.StandingPlan), &class)
		if err != nil {
			return err
		}
		r.Terms.StandingPlan, r.StandingPlan = &frozen, nil
	}
	return nil
}

// authoredStandingPhases projects a request's standing plan into the
// freeze's input, field for field, as the runner projects update_plan phases
// (runner.AuthoredPhasesFrom).
func authoredStandingPhases(in []domain.StandingPhase) []plangate.AuthoredPhase {
	out := make([]plangate.AuthoredPhase, 0, len(in))
	for _, p := range in {
		a := plangate.AuthoredPhase{ID: p.ID, Label: p.Label, Why: p.Why}
		for _, r := range p.Requires {
			a.Requires = append(a.Requires, plangate.AuthoredRequires{Phase: r.Phase, Why: r.Why})
		}
		if p.Max != nil {
			a.Max = &plangate.AuthoredMax{Count: p.Max.Count, Why: p.Max.Why}
		}
		if p.Budget != nil {
			a.Budget = &plangate.AuthoredBudget{Calls: p.Budget.Calls, Why: p.Budget.Why}
		}
		for _, h := range p.Permissions {
			a.Permissions = append(a.Permissions, plangate.AuthoredPermission{Handle: h.Handle, Why: h.Why})
		}
		for _, sl := range p.Slots {
			a.Slots = append(a.Slots, plangate.AuthoredSlot{Type: sl.Type, ID: sl.ID, Why: sl.Why})
		}
		out = append(out, a)
	}
	return out
}

var errGroupEvent = fmt.Errorf("%w: event watches are not available on group goals", domain.ErrDenied)

// definitiveGroupRefusal reports whether a group validation error means the
// approved terms can never run again without a fresh approval: an ErrDenied
// that is neither a membership sync lag nor a failed lookup. That covers the
// class, Channel or AgentIdentity changed or deleted since review, skills or
// guards changed, the kind no longer hosting group goals, the destination no
// longer the goal's group, and the approver refused by the class start gate.
func definitiveGroupRefusal(err error) bool {
	return errors.Is(err, domain.ErrDenied) && !errors.Is(err, domain.ErrMembershipNotCurrent) && !errors.Is(err, domain.ErrLookupFailed)
}

// groupLookupError refuses because what (a pinned object) could not be read:
// definitively (wrapping gone, the cause a dispatcher records) when it no
// longer exists, retryably otherwise.
func groupLookupError(what string, gone, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: %w: %s no longer exists", domain.ErrDenied, gone, what)
	}
	return fmt.Errorf("%w: %s: %w: %v", domain.ErrDenied, what, domain.ErrLookupFailed, err)
}

// validateGroup checks a group goal's execution terms against the live
// class, Channel and AgentIdentity. decider is the manager deciding the
// consent or renewal (at request time, the requesting member); the class
// start gate is checked for them, and an empty decider is refused.
func (s *Server) validateGroup(ctx context.Context, g domain.Goal, t domain.ExecutionTerms, decider identity.CanonicalUserID) error {
	if decider.IsZero() {
		return fmt.Errorf("%w: group goal execution needs a deciding manager", domain.ErrDenied)
	}
	return s.checkGroupTerms(ctx, g, t, func(class *v1.AgentClass) error {
		if err := s.checkStartGate(ctx, g.Domain, class, decider); err != nil {
			return err
		}
		return s.ReadGoal(ctx, domain.Actor{Domain: g.Domain, User: decider.String()}, g)
	})
}

// ValidateGroupDispatch implements domain.GroupDispatchValidator: the group
// rules for a run of an already-approved consent. Everything pinned at review
// is re-checked (class UID and digest, Channel, member ref, AgentIdentity,
// skills, guards, bounds, standing plan), but nothing about the approving
// manager: the start gate is checked for the decider at consent and renewal
// only, and what a run may post is the info-leakage gate's call.
// The approver's per-run eligibility — still a manager, still holding the
// slot permissions — is derivePlanApproval's, and failing it asks a human.
func (s *Server) ValidateGroupDispatch(ctx context.Context, g domain.Goal, t domain.ExecutionTerms) error {
	if !g.Domain.GroupOwned() {
		return domain.ErrDenied
	}
	return s.checkGroupTerms(ctx, g, t, nil)
}

// checkGroupTerms is validateGroup without the decider. deciderChecks, when
// set, runs at the point the decider's start gate and source reads belong.
func (s *Server) checkGroupTerms(ctx context.Context, g domain.Goal, t domain.ExecutionTerms, deciderChecks func(*v1.AgentClass) error) error {
	if s.Auth == nil || s.Reader == nil {
		return domain.ErrDenied
	}
	if t.Group == nil || t.Group.MemberGroup != g.Domain.Owner || t.Destination != (domain.PrivateDestination{}) {
		return fmt.Errorf("%w: execution must deliver to the group that owns this goal", domain.ErrDenied)
	}
	if t.Event != nil {
		return errGroupEvent
	}
	ns := g.Domain.Namespace
	var class v1.AgentClass
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: g.Domain.Class}, &class); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %w: execution class %q no longer exists", domain.ErrDenied, domain.ErrGroupNotHostable, g.Domain.Class)
		}
		return err
	}
	hash, err := digest(class.Spec)
	if err != nil {
		return err
	}
	if string(class.UID) != g.Domain.ClassUID || !class.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: %w: execution class changed since review", domain.ErrDenied, domain.ErrGroupNotHostable)
	}
	if hash != t.ClassDigest {
		return fmt.Errorf("%w: execution class changed since review", domain.ErrDenied)
	}

	// Destination: the pinned origin Channel, still hosting group goals, and
	// a member ref of a type its kind contributes. The origin session is not
	// consulted.
	var ch v1.Channel
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: t.Group.Channel}, &ch); err != nil {
		return groupLookupError(fmt.Sprintf("the group's channel %q", t.Group.Channel), domain.ErrGroupChannelGone, err)
	}
	if string(ch.UID) != t.Group.ChannelUID || !ch.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: %w: the group's channel %q changed since review", domain.ErrDenied, domain.ErrGroupChannelGone, t.Group.Channel)
	}
	if err := s.groupQualifies(ctx, &class, &ch); err != nil {
		return err
	}
	host, _ := registry.GroupGoalHostFor(ch.Spec.Kind) // groupQualifies established it
	typ, _, rel, ok := g.Domain.OwnerSubjectSet()
	if !ok || !slices.Contains(host.GroupMemberLinks(), typ+"#"+rel) {
		return fmt.Errorf("%w: %w: the goal's group is not a %s group", domain.ErrDenied, domain.ErrGroupNotHostable, ch.Spec.Kind)
	}

	if err := s.checkAgentIdentityPin(ctx, ns, &class, t); err != nil {
		return err
	}
	if err := checkSkillsPinned(&class, t); err != nil {
		return err
	}
	if err := checkExecutionGuards(&class); err != nil {
		return err
	}
	if deciderChecks != nil {
		if err := deciderChecks(&class); err != nil {
			return err
		}
	}
	if err := checkClassBounds(&class, t.Bounds); err != nil {
		return err
	}
	if t.StandingPlan != nil {
		frozen, err := freezeStandingPlan(*t.StandingPlan, &class)
		if err != nil {
			return err
		}
		if frozen.Digest() != t.StandingPlan.Digest() {
			return fmt.Errorf("%w: the standing plan is not in frozen form", domain.ErrInvalid)
		}
	}
	if t.Report != nil {
		if err := t.Report.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// checkAgentIdentityPin replaces the private owner-catalog pin: the terms name
// the class's spec.agentIdentity and pin the object's UID, so a recreated
// identity (new credentials) needs a fresh consent.
func (s *Server) checkAgentIdentityPin(ctx context.Context, ns string, class *v1.AgentClass, t domain.ExecutionTerms) error {
	if t.AgentIdentity != class.Spec.AgentIdentity {
		return fmt.Errorf("%w: %w: the agent's AgentIdentity changed since review", domain.ErrDenied, domain.ErrIdentityChanged)
	}
	if class.Spec.AgentIdentity == "" {
		if t.AgentIdentityUID != "" {
			return fmt.Errorf("%w: %w: the agent's AgentIdentity changed since review", domain.ErrDenied, domain.ErrIdentityChanged)
		}
		return nil
	}
	var ai v1.AgentIdentity
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: ns, Name: class.Spec.AgentIdentity}, &ai); err != nil {
		return groupLookupError(fmt.Sprintf("AgentIdentity %q", class.Spec.AgentIdentity), domain.ErrIdentityChanged, err)
	}
	if ai.UID == "" || string(ai.UID) != t.AgentIdentityUID || !ai.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: %w: the agent's AgentIdentity changed since review", domain.ErrDenied, domain.ErrIdentityChanged)
	}
	return nil
}

// freezeStandingPlan re-freezes a standing plan already in terms, the check
// validation runs on stored terms. A request's plan is frozen from its
// authored form by freezeAuthoredStandingPlan instead.
//
// Stored terms carry the plan in frozen shape, so it is projected back to authored phases — phase
// ids are the indices, requires edges name them — and re-frozen. Any freeze
// problem (an unparseable handle, a slot type the class never declared, an
// edge to a missing phase) refuses the plan rather than silently narrowing
// it. Freezing is idempotent, so validation re-freezes and compares digests.
//
// The goal server has no live permission surface (that is the runner's), so
// handles are parsed but not checked against one; a handle no tool reaches
// only narrows what the plan grants.
func freezeStandingPlan(p plangate.Plan, class *v1.AgentClass) (plangate.Plan, error) {
	authored := make([]plangate.AuthoredPhase, len(p.Phases))
	for i, ph := range p.Phases {
		// Consents are operator-prepared authority; a standing plan arrives
		// from the model, so one carrying them is refused, never copied.
		if len(ph.Consents) > 0 {
			return plangate.Plan{}, fmt.Errorf("%w: standing plan phase %d carries consents, which only the operator prepares", domain.ErrInvalid, i)
		}
		a := plangate.AuthoredPhase{ID: strconv.Itoa(i), Label: ph.Label, Why: ph.Why}
		if ph.Max != (plangate.MaxSpec{}) {
			a.Max = &plangate.AuthoredMax{Count: ph.Max.Count, Why: ph.Max.Why}
		}
		if ph.Budget != (plangate.BudgetSpec{}) {
			a.Budget = &plangate.AuthoredBudget{Calls: ph.Budget.Calls, Why: ph.Budget.Why}
		}
		for _, h := range ph.Permissions {
			a.Permissions = append(a.Permissions, plangate.AuthoredPermission{Handle: h.String()})
		}
		for _, sl := range ph.Slots {
			a.Slots = append(a.Slots, plangate.AuthoredSlot{Type: sl.Type, ID: sl.ID, Why: sl.Why})
		}
		for _, r := range ph.Requires {
			a.Requires = append(a.Requires, plangate.AuthoredRequires{Phase: strconv.Itoa(r.Phase), Why: r.Why})
		}
		authored[i] = a
	}
	return freezeAuthoredStandingPlan(authored, class)
}

// freezeAuthoredStandingPlan freezes an authored standing plan and refuses
// one no run could execute as approved: a freeze problem, no phase that
// reports the result, or two instances of one slot type (a run holds one).
func freezeAuthoredStandingPlan(authored []plangate.AuthoredPhase, class *v1.AgentClass) (plangate.Plan, error) {
	frozen, problems := plangate.FreezeFrom(authored, nil, class.Spec.SlotResourceTypes())
	if len(problems) > 0 {
		details := make([]string, len(problems))
		for i, pr := range problems {
			details[i] = fmt.Sprintf("phase %d: %s", pr.PhaseIndex, pr.Detail)
		}
		return plangate.Plan{}, fmt.Errorf("%w: standing plan: %s", domain.ErrInvalid, strings.Join(details, "; "))
	}
	// A run reports its result (respond_to_user, report_goal_result) under
	// perm:execute:agent_goal_execution. A standing plan without it could never
	// cover a run's delivery phase, so every run would stop at a card; refuse
	// the plan at consent instead of approving one that cannot run unattended.
	if !reportsResult(frozen) {
		return plangate.Plan{}, fmt.Errorf("%w: a group goal's standing plan must include a phase that reports its result (permission %s)", domain.ErrInvalid, resultHandle)
	}
	if conflicts := plangate.SlotInstanceConflicts(frozen); len(conflicts) > 0 {
		details := make([]string, len(conflicts))
		for i, c := range conflicts {
			details[i] = c.RefusalText()
		}
		return plangate.Plan{}, fmt.Errorf("%w: standing plan: %s", domain.ErrInvalid, strings.Join(details, "; "))
	}
	return frozen, nil
}

// resultHandle is the permission a goal run's result-reporting tools require.
const resultHandle = "perm:execute:" + ExecutionResourceType

func reportsResult(p plangate.Plan) bool {
	for _, ph := range p.Phases {
		for _, h := range ph.Permissions {
			if h.String() == resultHandle {
				return true
			}
		}
	}
	return false
}
