package goals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	domain "github.com/authzed/openagentprimitives/pkg/agent/goals"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
	"github.com/authzed/openagentprimitives/pkg/channels/channelevents"
	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/platform/identity"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The kind-neutral management API: list a viewer's group goals,
// view one's standing plan, and pause, resume, renew or remove it, from a
// first-party surface outside any conversation (a channel kind's home view).
//
// It is served to channelsd alone. The viewer is channelsd's word for a
// person the kind verified; the goal server never takes an actor from the
// request. It builds the session-less first-party actor (Actor.FirstParty)
// itself, after checking channelsd's token, and every decision about that
// actor is a live SpiceDB check on the viewer: view_memory to see a goal,
// pause to pause, manage on the goal (its listed managers still in the group,
// or a platform admin) for everything else.
//
// Resume and renew are approvals, and the click is the decision. The goal
// server requests the consent (re-approving the last approved terms, or
// renewing), hosts it at the goal's management host instead of a session,
// and returns the operator-signed request; channelsd records the viewer's
// decision on it and commits it exactly as for a consent card
// (pipeline.DecideGoalConsentFirstParty), so the decision is the same signed
// record, checked by the same CheckDecision and DecideExecution — the deciding
// manager's start gate included. Nothing is posted into the conversation.

// defaultManageLimit is a list's row cap when the surface names none.
const defaultManageLimit = 20

// firstPartyAttestation is a first-party actor's authorship evidence, kept
// in each audit event it causes where a session's actor keeps its signed
// channelsd entry.
//
// It is NOT a signed entry and must not read as one. The goal server builds
// it (RecordedBy), from a request authenticated only by channelsd's bearer
// token (Via), whose word names Viewer. So it has no "publisher" (the field a
// signed entry's provenance names its signer by) and no "provenance", and
// says Unsigned outright: an auditor weighs it as the operator's record of
// channelsd's claim, not as channelsd's signature.
type firstPartyAttestation struct {
	RecordedBy string    `json:"recordedBy"`
	Via        string    `json:"via"`
	Unsigned   bool      `json:"unsigned"`
	Surface    string    `json:"surface"`
	Viewer     string    `json:"viewer"`
	Goal       string    `json:"goal"`
	Action     string    `json:"action"`
	At         time.Time `json:"at"`
}

func (s *Server) manageHTTP(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || s.Tokens == nil || !s.Tokens.IsChannelsdToken(bearer) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.Auth == nil || s.Service == nil || s.Service.Store == nil {
		http.Error(w, "goal management unavailable", http.StatusServiceUnavailable)
		return
	}
	var req domain.ManageRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid management request", http.StatusBadRequest)
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid management request", http.StatusBadRequest)
		return
	}
	if req.Viewer == "" || req.Namespace == "" {
		http.Error(w, "invalid management request: viewer and namespace are required", http.StatusBadRequest)
		return
	}
	// The viewer is a person: a bare canonical user id. A subject of any
	// other type — a run session, a subject set — is never a management
	// viewer (group runs act as their session, and manage nothing).
	if strings.ContainsAny(req.Viewer, ":#/*") {
		http.Error(w, "invalid management request: viewer must be a person's canonical id", http.StatusBadRequest)
		return
	}
	ctx := memory.WithCaller(memory.WithSystemApproval(r.Context(), "system:operator"), "system:operator")
	viewer := identity.CanonicalFromTrusted(req.Viewer, "channelsd-verified management viewer")
	var out domain.ManageResponse
	var err error
	switch req.Operation {
	case domain.ManageList:
		out.Goals, out.Total, err = s.listForViewer(ctx, viewer, req.Namespace, req.Class, req.Limit)
	case domain.ManagePlan:
		out.Plan, out.Title, out.Digest, err = s.standingPlanFor(ctx, viewer, req.Namespace, req.Goal, req.Action)
	case domain.ManageAct:
		out.Approval, err = s.manageAct(ctx, viewer, req.Namespace, req.Goal, req.Action, req.Digest)
	default:
		err = fmt.Errorf("%w: unknown management operation %q", domain.ErrInvalid, req.Operation)
	}
	if err != nil {
		log.FromContext(ctx).Info("goal management refused", "operation", req.Operation, "viewer", req.Viewer, "namespace", req.Namespace, "goal", req.Goal, "action", req.Action, "error", err)
		if errors.Is(err, domain.ErrPlanChanged) {
			// Its own status, so the surface can say "reopen it" rather than
			// a generic conflict.
			http.Error(w, err.Error(), http.StatusPreconditionFailed)
			return
		}
		s.failGroup(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.FromContext(ctx).Info("goal management response failed", "operation", req.Operation, "viewer", req.Viewer, "goal", req.Goal, "error", err)
	}
}

// domainLister is the store's read by domain-ID set.
func (s *Server) domainLister() (domain.DomainListStore, error) {
	lister, ok := s.Service.Store.(domain.DomainListStore)
	if !ok {
		return nil, fmt.Errorf("%w: this goal store cannot list goals for a management surface", domain.ErrDenied)
	}
	return lister, nil
}

// viewerDomains are the agent_goal_domains viewer may view, from SpiceDB.
func (s *Server) viewerDomains(ctx context.Context, viewer identity.CanonicalUserID) ([]string, error) {
	ids, truncated, err := s.Auth.LookupResourceIDs(ctx, ResourceType, "view_memory", viewer)
	if err != nil {
		return nil, fmt.Errorf("looking up the goal domains %s may view: %w", viewer, err)
	}
	if truncated {
		log.FromContext(ctx).Info("goal management: the viewer's goal domains were truncated; the list shows the first ones only", "viewer", viewer.String(), "domains", len(ids))
	}
	if len(ids) > domain.MaxDomainSet {
		ids = ids[:domain.MaxDomainSet]
	}
	return ids, nil
}

// firstPartyActor is the session-less management actor for viewer on g.
func firstPartyActor(viewer identity.CanonicalUserID, g domain.Goal, action string) (domain.Actor, error) {
	att, err := json.Marshal(firstPartyAttestation{RecordedBy: "system:operator", Via: "channelsd-token", Unsigned: true, Surface: "management", Viewer: viewer.String(), Goal: g.ID, Action: action, At: time.Now().UTC()})
	if err != nil {
		return domain.Actor{}, fmt.Errorf("goal %s: encoding the management attestation: %w", g.ID, err)
	}
	return domain.Actor{Domain: g.Domain, User: viewer.String(), FirstParty: true, Proof: "first-party:" + action, Attestation: string(att)}, nil
}

// listForViewer lists the group goals of class in ns that viewer can see:
// the domains SpiceDB says viewer may view, read by domain-ID set, minus
// finished goals and goals with a source viewer cannot read. total counts
// them all; rows are the first limit, summarized for viewer.
func (s *Server) listForViewer(ctx context.Context, viewer identity.CanonicalUserID, ns, class string, limit int) ([]domain.GoalSummary, int, error) {
	if limit == 0 {
		limit = defaultManageLimit
	}
	if limit < 1 || limit > 100 {
		return nil, 0, fmt.Errorf("%w: limit must be 1-100", domain.ErrInvalid)
	}
	lister, err := s.domainLister()
	if err != nil {
		return nil, 0, err
	}
	ids, err := s.viewerDomains(ctx, viewer)
	if err != nil {
		return nil, 0, err
	}
	var visible []domain.Goal
	for r := (domain.ListRequest{Limit: 100}); ; {
		p, err := lister.ListDomains(ctx, ids, r)
		if err != nil {
			return nil, 0, fmt.Errorf("listing goals for %s: %w", viewer, err)
		}
		for _, g := range p.Goals {
			if g.Domain.Namespace != ns || (class != "" && g.Domain.Class != class) || !g.Domain.GroupOwned() || g.State == domain.Completed || g.State == domain.Cancelled {
				continue
			}
			a, err := firstPartyActor(viewer, g, domain.ManageList)
			if err != nil {
				return nil, 0, err
			}
			// As Service.List: a goal resting on a source viewer cannot read
			// is not theirs to see.
			if err := s.ReadGoal(ctx, a, g); err != nil {
				if errors.Is(err, domain.ErrDenied) {
					continue
				}
				return nil, 0, fmt.Errorf("goal %s for %s: %w", g.ID, viewer, err)
			}
			visible = append(visible, g)
		}
		if p.Next == "" {
			break
		}
		r.After = p.Next
	}
	rows := make([]domain.GoalSummary, 0, min(limit, len(visible)))
	for _, g := range visible[:min(limit, len(visible))] {
		row, err := s.summarize(ctx, viewer, g)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, row)
	}
	return rows, len(visible), nil
}

// summarize is g as a management surface lists it for viewer.
func (s *Server) summarize(ctx context.Context, viewer identity.CanonicalUserID, g domain.Goal) (domain.GoalSummary, error) {
	now := s.serviceNow()
	a, err := firstPartyActor(viewer, g, domain.ManageList)
	if err != nil {
		return domain.GoalSummary{}, err
	}
	row := domain.GoalSummary{Resource: GoalResourceType + ":" + g.ID, Title: g.Title, Group: g.Domain.Owner, State: string(g.State),
		BlockedReason: domain.BlockedReasonAt(g, now), Managers: slices.Clone(g.Managers)}
	if row.CanPause, err = s.checkDomain(ctx, a, "pause"); err != nil {
		return domain.GoalSummary{}, fmt.Errorf("goal %s: checking pause for %s: %w", g.ID, viewer, err)
	}
	switch err := s.AuthorizeGoalManager(ctx, a, g); {
	case err == nil:
		row.CanManage = true
	case !errors.Is(err, domain.ErrDenied):
		return domain.GoalSummary{}, fmt.Errorf("goal %s: checking manage for %s: %w", g.ID, viewer, err)
	}
	inGroup, err := s.managerInGroup(ctx, g)
	if err != nil {
		return domain.GoalSummary{}, err
	}
	row.NoManagerInGroup = !inGroup

	inForce := domain.ApprovalInForce(g, now)
	if c := g.Execution; c != nil && c.Decision != nil && c.Decision.Approved {
		expires := c.Terms.ExpiresAt
		row.ApprovalExpires = &expires
	}
	if inForce {
		windows, err := g.Execution.Terms.ExecutionWindows()
		if err != nil {
			return domain.GoalSummary{}, fmt.Errorf("goal %s: resolving its approved runs: %w", g.ID, err)
		}
		for _, w := range windows {
			if w.DueAt.After(now) {
				due := w.DueAt
				row.NextRun = &due
				break
			}
		}
	}
	terms := g.LastApprovedTerms
	if g.Execution != nil {
		terms = &g.Execution.Terms
	}
	if terms != nil {
		if row.Schedule, err = describeSchedule(*terms); err != nil {
			return domain.GoalSummary{}, fmt.Errorf("goal %s: describing its schedule: %w", g.ID, err)
		}
	}
	row.RenewalPending = domain.RenewalPending(g) && g.PendingRenewal.Decision == nil
	scheduled := g.Execution != nil && g.Execution.Terms.Schedule != nil
	row.CanRenew = row.CanManage && g.State == domain.Active &&
		(row.RenewalPending || (inForce && scheduled) || (!inForce && g.LastApprovedTerms != nil)) &&
		approvable(g, domain.ActRenew, now)
	row.CanResume = row.CanManage && g.State == domain.Paused && approvable(g, domain.ActResume, now)
	return row, nil
}

// approvable reports whether action (resume or renew) on g could be approved
// from a management surface at now: what it would approve has a standing plan
// to show, and terms that would be asked for again can still be offered (a
// one-time run not yet due, a schedule with a run left). A surface offers the
// action only then. It is advisory: the act re-checks everything.
func approvable(g domain.Goal, action string, now time.Time) bool {
	target, err := selectApproval(g, action, now)
	if err != nil || target.terms.StandingPlan == nil {
		return false
	}
	switch target.source {
	case approveLastTerms:
		_, err = freshLastApproved(target.terms, now)
	case approveRenewal:
		_, err = domain.RenewedTerms(target.terms, now)
	}
	return err == nil
}

// managerInGroup reports whether any of g's managers — listed on the goal
// and holding its manager tuple — is still in its group: whether anyone but
// a platform admin can manage it.
func (s *Server) managerInGroup(ctx context.Context, g domain.Goal) (bool, error) {
	have, err := s.Auth.ListUserSubjects(ctx, GoalResourceType, g.ID, "manager")
	if err != nil {
		return false, fmt.Errorf("goal %s: reading its managers: %w", g.ID, err)
	}
	for _, m := range g.Managers {
		if !slices.Contains(have, m) {
			continue
		}
		ok, err := s.isMember(ctx, g.Domain, m)
		if err != nil {
			return false, fmt.Errorf("goal %s: checking whether manager %s is in its group: %w", g.ID, m, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// describeSchedule says when t runs, for a list row.
func describeSchedule(t domain.ExecutionTerms) (string, error) {
	if t.Schedule == nil {
		return "once at " + t.DueAt.UTC().Format("2 Jan 2006, 15:04 UTC"), nil
	}
	description, err := sessionschedule.Describe(*t.Schedule)
	if err != nil {
		return "", err
	}
	loc, err := time.LoadLocation(t.Schedule.Timezone)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s at %s · %s", description, t.DueAt.In(loc).Format("15:04"), t.Schedule.Timezone), nil
}

// findGoal reads the goal resource names, among the domains viewer may view,
// in namespace ns. Anything else — malformed, unknown, invisible, another
// namespace, a private goal — is ErrNotFound, so the API never confirms a
// goal the viewer cannot see.
func (s *Server) findGoal(ctx context.Context, viewer identity.CanonicalUserID, ns, resource string) (domain.Goal, error) {
	id, ok := strings.CutPrefix(resource, GoalResourceType+":")
	if !ok || id == "" {
		return domain.Goal{}, fmt.Errorf("%w: %q does not name a goal (agent_goal:<id>)", domain.ErrInvalid, resource)
	}
	lister, err := s.domainLister()
	if err != nil {
		return domain.Goal{}, err
	}
	ids, err := s.viewerDomains(ctx, viewer)
	if err != nil {
		return domain.Goal{}, err
	}
	g, err := lister.GetInDomains(ctx, ids, id)
	if err != nil {
		return domain.Goal{}, err
	}
	if g.Domain.Namespace != ns || !g.Domain.GroupOwned() {
		return domain.Goal{}, domain.ErrNotFound
	}
	return g, nil
}

// standingPlanFor is the plan the goal resource names shows before action,
// with the goal's title and, for an approval (resume, renew), the binding
// digest the act must hand back.
//
// For resume and renew it is exactly what that act would approve now
// (selectApproval, the selection the act itself runs), so a manager is never
// shown one plan and asked to approve another. For View plan (no action) and
// the other actions it is the plan in force or awaiting approval, else the
// one the goal last ran under; only View plan treats "none" as an error.
func (s *Server) standingPlanFor(ctx context.Context, viewer identity.CanonicalUserID, ns, resource, action string) (*plangate.Plan, string, string, error) {
	g, err := s.findGoal(ctx, viewer, ns, resource)
	if err != nil {
		return nil, "", "", err
	}
	a, err := firstPartyActor(viewer, g, domain.ManagePlan)
	if err != nil {
		return nil, "", "", err
	}
	if err := s.ReadGoal(ctx, a, g); err != nil {
		return nil, "", "", err
	}
	switch action {
	case domain.ActResume, domain.ActRenew:
		target, err := selectApproval(g, action, s.serviceNow())
		if err != nil {
			return nil, "", "", err
		}
		if target.terms.StandingPlan == nil {
			// The goal exists and the viewer can see it: ErrInvalid, so a
			// surface does not report it as gone.
			return nil, "", "", fmt.Errorf("%w: goal %s has no standing plan to approve from a management surface; ask the agent in its conversation to request approval", domain.ErrInvalid, g.ID)
		}
		binding, err := approvalBinding(target.terms)
		if err != nil {
			return nil, "", "", fmt.Errorf("goal %s: digesting the plan %s approves: %w", g.ID, action, err)
		}
		plan := *target.terms.StandingPlan
		return &plan, g.Title, binding, nil
	case "", domain.ActPause, domain.ActRemove:
	default:
		return nil, "", "", fmt.Errorf("%w: unknown goal action %q (pause, resume, renew or remove)", domain.ErrInvalid, action)
	}
	for _, t := range []*domain.ExecutionTerms{executionTerms(g.Execution), executionTerms(g.PendingRenewal), g.LastApprovedTerms} {
		if t != nil && t.StandingPlan != nil {
			plan := *t.StandingPlan
			return &plan, g.Title, "", nil
		}
	}
	if action != "" {
		return nil, g.Title, "", nil
	}
	return nil, "", "", fmt.Errorf("%w: goal %s has no standing plan", domain.ErrNotFound, g.ID)
}

func executionTerms(c *domain.ExecutionConsent) *domain.ExecutionTerms {
	if c == nil {
		return nil
	}
	return &c.Terms
}

// approvalSource is which consent an approving management act decides.
type approvalSource int

const (
	// approveNothing: resume on a goal whose approval is in force; there is
	// nothing left to approve.
	approveNothing approvalSource = iota
	// approvePendingRenewal: the undecided renewal of the approval in force.
	approvePendingRenewal
	// approveRenewal: a new renewal of the approval in force.
	approveRenewal
	// approveOpenRequest: the undecided request an earlier click left at the
	// goal's management host.
	approveOpenRequest
	// approveLastTerms: the terms the goal last ran under, asked for again
	// with a fresh window.
	approveLastTerms
)

// approvalTarget is what an approving act decides, and the terms it decides.
// For a renewal or a fresh request the terms' window moves when the request
// is made; approvalBinding leaves the window out for that reason.
type approvalTarget struct {
	source approvalSource
	terms  domain.ExecutionTerms
}

// selectApproval is what resume or renew on g approves at now — the ONE
// selection both the plan read a manager is shown and the act itself use, so
// the two cannot disagree. It reads only g: a request whose card has lapsed,
// or whose record is gone, is found by the act, which then falls through to
// renewalFallback and re-checks the binding against that.
func selectApproval(g domain.Goal, action string, now time.Time) (approvalTarget, error) {
	switch action {
	case domain.ActResume:
		if g.LastApprovedTerms == nil {
			return approvalTarget{}, fmt.Errorf("%w: goal %s has never been approved, so there is nothing to resume it under; ask the agent in its conversation to request approval", domain.ErrInvalid, g.ID)
		}
		switch g.State {
		case domain.Paused:
			// Resuming bumps the revision, so no earlier request or approval
			// survives it: the re-approval is of the last approved terms.
			return approvalTarget{source: approveLastTerms, terms: *g.LastApprovedTerms}, nil
		case domain.Active:
			return lastTermsTarget(g, now)
		}
		return approvalTarget{}, fmt.Errorf("%w: goal %s is %s; only a paused goal can be resumed", domain.ErrInvalid, g.ID, g.State)
	case domain.ActRenew:
		if g.State != domain.Active {
			return approvalTarget{}, fmt.Errorf("%w: goal %s is %s; only an active goal can be renewed (resume a paused one)", domain.ErrInvalid, g.ID, g.State)
		}
		if domain.RenewalPending(g) && g.PendingRenewal.Decision == nil {
			return approvalTarget{source: approvePendingRenewal, terms: g.PendingRenewal.Terms}, nil
		}
		return renewalFallback(g, now)
	}
	return approvalTarget{}, fmt.Errorf("%w: %q is not an approval", domain.ErrInvalid, action)
}

// renewalFallback is what renew approves when no pending renewal can be
// decided: a renewal of the approval in force, else the last approved terms.
func renewalFallback(g domain.Goal, now time.Time) (approvalTarget, error) {
	if domain.ApprovalInForce(g, now) {
		return approvalTarget{source: approveRenewal, terms: g.Execution.Terms}, nil
	}
	return lastTermsTarget(g, now)
}

// lastTermsTarget is what re-approving active goal g's last approved terms
// decides: nothing with an approval in force; the undecided request an
// earlier click left at the management host; else the last approved terms.
func lastTermsTarget(g domain.Goal, now time.Time) (approvalTarget, error) {
	if domain.ApprovalInForce(g, now) {
		return approvalTarget{source: approveNothing, terms: g.Execution.Terms}, nil
	}
	if c := g.Execution; c != nil && c.Decision == nil && c.ManagementHosted() && g.Revision == c.RequestRevision && now.Before(c.Terms.ExpiresAt) {
		return approvalTarget{source: approveOpenRequest, terms: c.Terms}, nil
	}
	if g.LastApprovedTerms == nil {
		return approvalTarget{}, fmt.Errorf("%w: goal %s has never been approved; ask the agent in its conversation to request approval", domain.ErrInvalid, g.ID)
	}
	return approvalTarget{source: approveLastTerms, terms: *g.LastApprovedTerms}, nil
}

// approvalBinding digests what approving t approves: every term but its
// window (DueAt, ExpiresAt, ScheduleWindows), which a renewal or a fresh
// request moves forward on its own when it is made. It binds a surface's
// approval to the plan it showed; it is never authority — the act still
// checks the viewer, and the decision is still the signed record.
func approvalBinding(t domain.ExecutionTerms) (string, error) {
	t.DueAt, t.ExpiresAt, t.ScheduleWindows = time.Time{}, time.Time{}, nil
	d, err := digest(t)
	if err != nil {
		return "", err
	}
	return "approval-binding:" + d, nil
}

// checkShown refuses target unless shown is its binding: the act would
// approve something other than what the viewer was shown.
func checkShown(g domain.Goal, target approvalTarget, shown string) error {
	binding, err := approvalBinding(target.terms)
	if err != nil {
		return fmt.Errorf("goal %s: digesting the plan to approve: %w", g.ID, err)
	}
	if shown == "" || shown != binding {
		return fmt.Errorf("%w: goal %s: the plan changed since you opened this; reopen it to see the plan you would approve", domain.ErrPlanChanged, g.ID)
	}
	return nil
}

// manageAct takes action on the goal resource names, as viewer. For resume
// and renew it returns the consent request viewer's click decides, refused
// unless shown is the binding of what it approves.
func (s *Server) manageAct(ctx context.Context, viewer identity.CanonicalUserID, ns, resource, action, shown string) (*memory.Entry, error) {
	g, err := s.findGoal(ctx, viewer, ns, resource)
	if err != nil {
		return nil, err
	}
	a, err := firstPartyActor(viewer, g, action)
	if err != nil {
		return nil, err
	}
	switch action {
	case domain.ActPause:
		return nil, s.manageChange(ctx, a, g, "pause")
	case domain.ActRemove:
		return nil, s.manageChange(ctx, a, g, "cancel")
	case domain.ActResume:
		return s.resumeInModal(ctx, a, g, shown)
	case domain.ActRenew:
		return s.renewInModal(ctx, a, g, shown)
	}
	return nil, fmt.Errorf("%w: unknown goal action %q (pause, resume, renew or remove)", domain.ErrInvalid, action)
}

// manageRequestID is the idempotency key of a management change: the same
// click on the same goal state is the same request.
func manageRequestID(action string, g domain.Goal, extra any) (string, error) {
	id, err := digest(struct {
		Action   string
		Goal     string
		Revision int64
		Extra    any
	}{action, g.ID, g.Revision, extra})
	if err != nil {
		return "", fmt.Errorf("goal %s: deriving the %s request id: %w", g.ID, action, err)
	}
	return "manage-" + action + "-" + id, nil
}

// manageChange applies a goal update (pause, cancel) as the first-party
// actor, through the Service's own authorization, and reconciles the goal's
// tuples as every group write does.
func (s *Server) manageChange(ctx context.Context, a domain.Actor, g domain.Goal, action string) error {
	id, err := manageRequestID(action, g, nil)
	if err != nil {
		return err
	}
	next, err := s.Service.Update(ctx, a, domain.Change{RequestID: id, ID: g.ID, Revision: g.Revision, Action: action})
	if err != nil {
		return err
	}
	return s.syncGroupTuples(ctx, next)
}

// resumeInModal resumes paused goal g and re-approves the terms it last ran
// under, with a fresh window, as a's decision. The re-approval is checked
// first (manager, the plan shown, that the terms can be offered again, start
// gate, every pin), so a manager whose approval could not be recorded does
// not leave the goal half-resumed. A paused goal with no run left to approve
// (a one-time run whose time has passed, a schedule that has ended) is
// refused and stays paused: resumed, it would be active with no approval and
// nothing to ask for.
func (s *Server) resumeInModal(ctx context.Context, a domain.Actor, g domain.Goal, shown string) (*memory.Entry, error) {
	if err := s.AuthorizeGoalManager(ctx, a, g); err != nil {
		return nil, err
	}
	now := s.serviceNow()
	target, err := selectApproval(g, domain.ActResume, now)
	if err != nil {
		return nil, err
	}
	if err := checkShown(g, target, shown); err != nil {
		return nil, err
	}
	if g.State == domain.Paused {
		if _, err := reofferedTerms(g, now); err != nil {
			return nil, err
		}
		if err := s.ValidateGroupExecution(ctx, g, *g.LastApprovedTerms, a.User); err != nil {
			return nil, err
		}
		if err := s.manageChange(ctx, a, g, "resume"); err != nil {
			return nil, err
		}
		next, err := s.Service.Store.Get(ctx, g.Domain, g.ID)
		if err != nil {
			return nil, fmt.Errorf("goal %s resumed, but re-reading it failed; resume again to approve it: %w", g.ID, err)
		}
		g = next
	}
	if g.State != domain.Active {
		return nil, fmt.Errorf("%w: goal %s is %s; only a paused goal can be resumed", domain.ErrInvalid, g.ID, g.State)
	}
	return s.approveLastTerms(ctx, a, g, shown)
}

// renewInModal renews active goal g's approval as a's decision: the pending
// renewal if one awaits a manager, else a renewal of the approval in force,
// else (it ran out) a fresh approval of the terms it last ran under — the
// selection selectApproval makes, refused unless shown is its binding.
func (s *Server) renewInModal(ctx context.Context, a domain.Actor, g domain.Goal, shown string) (*memory.Entry, error) {
	if err := s.AuthorizeGoalManager(ctx, a, g); err != nil {
		return nil, err
	}
	now := s.serviceNow()
	target, err := selectApproval(g, domain.ActRenew, now)
	if err != nil {
		return nil, err
	}
	if err := checkShown(g, target, shown); err != nil {
		return nil, err
	}
	if target.source == approvePendingRenewal {
		entry, ok, err := s.decidableRequest(ctx, domain.RenewalView(g))
		if err != nil || ok {
			return entry, err
		}
		// Its card lapsed, or its record is gone with the run that hosted
		// it: renew as if none were pending, if that is still what was shown.
		if target, err = renewalFallback(g, now); err != nil {
			return nil, err
		}
		if err := checkShown(g, target, shown); err != nil {
			return nil, err
		}
	}
	if target.source != approveRenewal {
		return s.approveLastTerms(ctx, a, g, shown)
	}
	pending := ""
	if g.PendingRenewal != nil {
		pending = g.PendingRenewal.Digest
	}
	id, err := manageRequestID("renew", g, struct{ Execution, Pending string }{g.Execution.Digest, pending})
	if err != nil {
		return nil, err
	}
	renewed, err := s.Service.RenewExecution(ctx, a, domain.ExecutionRequest{RequestID: id, ID: g.ID, Revision: g.Revision})
	if err != nil {
		return nil, err
	}
	if renewed.PendingRenewal == nil {
		return nil, fmt.Errorf("%w: goal %s: the renewal was not recorded", domain.ErrConflict, g.ID)
	}
	return s.requireDecidable(ctx, domain.RenewalView(renewed))
}

// approveLastTerms requests active goal g's last approved terms again, with a
// fresh window, hosted at its management host, and returns the request for
// a to decide (lastTermsTarget). An undecided request an earlier click left
// there is offered again rather than asked twice. With an approval in force
// there is nothing to approve. Whatever it approves must be what was shown.
func (s *Server) approveLastTerms(ctx context.Context, a domain.Actor, g domain.Goal, shown string) (*memory.Entry, error) {
	now := s.serviceNow()
	target, err := lastTermsTarget(g, now)
	if err != nil {
		return nil, err
	}
	if err := checkShown(g, target, shown); err != nil {
		return nil, err
	}
	switch target.source {
	case approveNothing:
		return nil, nil
	case approveOpenRequest:
		entry, ok, err := s.decidableRequest(ctx, g)
		if err != nil || ok {
			return entry, err
		}
		// Lapsed: ask again for the last approved terms, if that is what
		// was shown.
		if g.LastApprovedTerms == nil {
			return nil, fmt.Errorf("%w: goal %s has never been approved; ask the agent in its conversation to request approval", domain.ErrInvalid, g.ID)
		}
		if err := checkShown(g, approvalTarget{source: approveLastTerms, terms: *g.LastApprovedTerms}, shown); err != nil {
			return nil, err
		}
	}
	terms, err := reofferedTerms(g, now)
	if err != nil {
		return nil, err
	}
	id, err := manageRequestID("approve", g, terms)
	if err != nil {
		return nil, err
	}
	requested, err := s.Service.RequestExecution(ctx, a, domain.ExecutionRequest{RequestID: id, ID: g.ID, Revision: g.Revision, Terms: terms})
	if err != nil {
		return nil, err
	}
	return s.requireDecidable(ctx, requested)
}

// reofferedTerms is what re-approving g's last approved terms asks for at now:
// those terms with a fresh window (freshLastApproved). It is ErrInvalid when
// they cannot be offered again — a one-time run whose time has passed, a
// schedule with no run left. Resume checks it before it commits and the
// re-approval uses it, so the two cannot disagree.
func reofferedTerms(g domain.Goal, now time.Time) (domain.ExecutionTerms, error) {
	if g.LastApprovedTerms == nil {
		return domain.ExecutionTerms{}, fmt.Errorf("%w: goal %s has never been approved; ask the agent in its conversation to request approval", domain.ErrInvalid, g.ID)
	}
	terms, err := freshLastApproved(*g.LastApprovedTerms, now)
	if err != nil {
		return domain.ExecutionTerms{}, fmt.Errorf("goal %s: its last approved terms cannot be offered again: %w", g.ID, err)
	}
	return terms, nil
}

// decidableRequest returns the operator-signed request for the consent in
// view.Execution while its card is still open (a decision after the card
// lapses would not verify). ok is false once lapsed.
//
// The record is written only into the management host. A consent hosted
// elsewhere — a renewal the operator asked for from a run, or one a manager
// asked for in their own conversation — is decided here only if its record
// is already there: its host scope may be gone with its run, and re-creating
// a record there would leave the decision in a scope nothing reads. ok is
// false then too, and the caller renews from the management host instead.
func (s *Server) decidableRequest(ctx context.Context, view domain.Goal) (*memory.Entry, bool, error) {
	if s.Consent == nil || s.Consent.Writer == nil || s.Consent.Memory == nil {
		return nil, false, fmt.Errorf("%w: approvals from a management surface are not served here", domain.ErrDenied)
	}
	var entry memory.Entry
	var request channelevents.InteractionRequestPayload
	var err error
	if view.Execution.ManagementHosted() {
		entry, request, err = s.Consent.EnsureRequest(ctx, view)
	} else {
		var found bool
		entry, request, found, err = s.Consent.FindRequest(ctx, view)
		if err == nil && !found {
			log.FromContext(ctx).Info("goal management: a pending consent's request record is not in its host; not re-creating it there",
				"goal", view.ID, "host", view.Execution.Session, "digest", view.Execution.Digest)
			return nil, false, nil
		}
	}
	if err != nil {
		return nil, false, err
	}
	// The wall clock, not serviceNow: a card's expiry is stamped from the
	// wall clock when its request is written (ConsentPublisher) and checked
	// against it when its decision is verified, so this must agree with those.
	if request.ExpiresAt == nil || !time.Now().Before(*request.ExpiresAt) {
		return nil, false, nil
	}
	return &entry, true, nil
}

// requireDecidable is decidableRequest for a request just made, which must
// still be open.
func (s *Server) requireDecidable(ctx context.Context, view domain.Goal) (*memory.Entry, error) {
	entry, ok, err := s.decidableRequest(ctx, view)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: goal %s: the approval request lapsed before it could be decided; try again", domain.ErrConflict, view.ID)
	}
	return entry, nil
}

// serviceNow is the Service's clock.
func (s *Server) serviceNow() time.Time {
	if s.Service != nil && s.Service.Now != nil {
		return s.Service.Now().UTC()
	}
	return time.Now().UTC()
}
