package goals

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
	"github.com/authzed/openagentprimitives/pkg/authz/plangate"
)

// ExecutionBounds are launch ceilings. They do not authorize external actions:
// each occurrence must get a fresh enforcing plan approval before acting.
type ExecutionBounds struct {
	DurationSeconds int64 `json:"durationSeconds"`
	Turns           int64 `json:"turns"`
	Tokens          int64 `json:"tokens"`
	ApprovalSeconds int64 `json:"approvalSeconds"`
}

// PrivateDestination pins the reviewed route and its intended human recipient.
// The authority adapter must resolve it through channel-kind contracts.
type PrivateDestination struct {
	BindingDigest string `json:"bindingDigest"`
	Channel       string `json:"channel"`
	ChannelUID    string `json:"channelUID"`
	Recipient     string `json:"recipient"`
}

// GroupDestination pins the reviewed route of a group-owned goal. MemberGroup
// must equal the goal domain's Owner subject-set.
type GroupDestination struct {
	Channel     string `json:"channel"`
	ChannelUID  string `json:"channelUID"`
	MemberGroup string `json:"memberGroup"`
}

// RouteDigest is what a group reply's delivery pins as its route. A private
// reply pins the reviewed binding's digest, but a group run's binding is
// rebuilt per run and re-anchored by its kind on first post, so a group reply
// pins the reviewed destination itself: Channel, its UID and the group.
func (d GroupDestination) RouteDigest() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "group-route:" + hex.EncodeToString(sum[:]), nil
}

type ExecutionTerms struct {
	Report *ReportPolicy `json:"report,omitempty"`
	// ActionApproval is independent of how the source consent is presented.
	// Empty/manual requires a fresh human decision; standing_private permits
	// derivation for the exact private reporting ceiling in each occurrence.
	ActionApproval string                `json:"actionApproval,omitempty"`
	Event          *EventWatch           `json:"event,omitempty"`
	Schedule       *sessionschedule.Spec `json:"schedule,omitempty"`
	// Windows are resolved by the server and committed to the reviewed digest.
	ScheduleWindows   []sessionschedule.Window `json:"scheduleWindows,omitempty"`
	SkillVersions     []string                 `json:"skillVersions,omitempty"`
	OwnerCatalogUID   string                   `json:"ownerCatalogUID"`
	ClassDigest       string                   `json:"classDigest"`
	DueAt             time.Time                `json:"dueAt"`
	ExpiresAt         time.Time                `json:"expiresAt"`
	Bounds            ExecutionBounds          `json:"bounds"`
	AllowedOperations []string                 `json:"allowedOperations"`
	Evidence          []string                 `json:"evidence"`
	Destination       PrivateDestination       `json:"destination"`
	// Group is the group-owned alternative to Destination; exactly one is set.
	Group *GroupDestination `json:"group,omitempty"`
	// StandingPlan is the server-frozen plan a group goal's runs derive from
	// when ActionApproval is standing_plan; the terms digest covers its digest.
	StandingPlan     *plangate.Plan `json:"standingPlan,omitempty"`
	AgentIdentity    string         `json:"agentIdentity,omitempty"`
	AgentIdentityUID string         `json:"agentIdentityUID,omitempty"`
}

type ExecutionRequest struct {
	ApprovalMode string         `json:"approvalMode,omitempty"`
	RequestID    string         `json:"requestID"`
	ID           string         `json:"id"`
	Revision     int64          `json:"revision"`
	Terms        ExecutionTerms `json:"terms"`
	// StandingPlan is a group goal's standing plan as the model authored it
	// (terms.standingPlan in request_goal_execution's arguments, the
	// update_plan phase shape). The goal server freezes it into
	// Terms.StandingPlan; Terms never arrives with a frozen plan from a
	// caller.
	StandingPlan []StandingPhase `json:"standingPlan,omitempty"`
}

// StandingPhase is one phase of an authored standing plan, in the shape
// update_plan's phases take. Everything in it is untrusted until frozen.
type StandingPhase struct {
	ID          string               `json:"id"`
	Label       string               `json:"label"`
	Why         string               `json:"why"`
	Requires    []StandingRequires   `json:"requires,omitempty"`
	Max         *StandingCount       `json:"max,omitempty"`
	Budget      *StandingBudget      `json:"budget,omitempty"`
	Permissions []StandingPermission `json:"permissions,omitempty"`
	Slots       []StandingSlot       `json:"slots,omitempty"`
}

// StandingRequires names a prerequisite phase by its authored id.
type StandingRequires struct {
	Phase string `json:"phase"`
	Why   string `json:"why"`
}

// StandingCount is a phase's entry budget.
type StandingCount struct {
	Count int    `json:"count"`
	Why   string `json:"why"`
}

// StandingBudget is a phase's governed-call budget.
type StandingBudget struct {
	Calls int    `json:"calls"`
	Why   string `json:"why"`
}

// StandingPermission is a permission handle a phase may use.
type StandingPermission struct {
	Handle string `json:"handle"`
	Why    string `json:"why"`
}

// StandingSlot is a concrete resource a phase acts on.
type StandingSlot struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Why  string `json:"why"`
}

type ExecutionConsent struct {
	Purpose         string             `json:"purpose,omitempty"`
	ApprovalMode    string             `json:"approvalMode,omitempty"`
	Session         string             `json:"session"`
	SessionUID      string             `json:"sessionUID"`
	RequestID       string             `json:"requestID"`
	RequestRevision int64              `json:"requestRevision"`
	Digest          string             `json:"digest"`
	Terms           ExecutionTerms     `json:"terms"`
	Decision        *ExecutionDecision `json:"decision,omitempty"`
	// Renews is the digest of the approval this consent renews; empty for
	// any other request.
	Renews string `json:"renews,omitempty"`
}

// ExecutionDecision is accepted only after the authority verifies its signed
// human interaction witness. This struct alone is never an authorization.
type ExecutionDecision struct {
	RequestID string `json:"requestID"`
	Digest    string `json:"digest"`
	Owner     string `json:"owner"`
	Approved  bool   `json:"approved"`
	Witness   string `json:"witness"`
}

// ExecutionAuthority is deliberately separate from management authority.
// Validate must check current policy ceilings, class version, private route,
// start permission, source access, and authoritative user status. VerifyDecision
// verifies a component-signed interaction, exact request digest and human owner.
// AuthorizeDispatch additionally requires the current SpiceDB execution grant;
// neither a database approval nor a tuple is sufficient on its own. Operators
// without all these collaborators must leave ExecutionAuth nil (fail closed).
type ExecutionAuthority interface {
	Validate(context.Context, Goal, ExecutionTerms) error
	VerifyDecision(context.Context, Goal, ExecutionDecision) error
	AuthorizeDispatch(context.Context, Goal) error
}

// GroupExecutionValidator is implemented by an ExecutionAuthority that serves
// group goals. Their rules check the class start gate for whoever is deciding
// — the requesting manager when the request is made, the clicking manager
// when it is approved — which Validate's signature cannot carry. An authority
// without it refuses group execution.
type GroupExecutionValidator interface {
	ValidateGroupExecution(ctx context.Context, g Goal, t ExecutionTerms, decider string) error
}

// GroupDispatchValidator is implemented by an ExecutionAuthority that runs
// group goals: the group rules for a run of an approved consent, which
// re-check every pinned object but not the approving manager (whose start
// gate is a consent-time check, and whose per-run eligibility decides plan
// approval, not dispatch). An authority without it refuses group dispatch.
type GroupDispatchValidator interface {
	ValidateGroupDispatch(ctx context.Context, g Goal, t ExecutionTerms) error
}

// validateDispatch runs the authority's rules for a run of g: Validate for a
// private goal, ValidateGroupDispatch for a group goal.
func (s *Service) validateDispatch(ctx context.Context, g Goal, t ExecutionTerms) error {
	if !g.Domain.GroupOwned() {
		return s.ExecutionAuth.Validate(ctx, g, t)
	}
	v, ok := s.ExecutionAuth.(GroupDispatchValidator)
	if !ok {
		return fmt.Errorf("%w: group goal dispatch is not served here", ErrDenied)
	}
	return v.ValidateGroupDispatch(ctx, g, t)
}

// validateExecution runs the authority's rules for g: Validate for a private
// goal, ValidateGroupExecution with decider for a group goal.
func (s *Service) validateExecution(ctx context.Context, g Goal, t ExecutionTerms, decider string) error {
	if !g.Domain.GroupOwned() {
		return s.ExecutionAuth.Validate(ctx, g, t)
	}
	v, ok := s.ExecutionAuth.(GroupExecutionValidator)
	if !ok {
		return fmt.Errorf("%w: group goal execution is not served here", ErrDenied)
	}
	return v.ValidateGroupExecution(ctx, g, t, decider)
}

// checkDecider requires the decision's Owner to be who may decide g: the goal
// owner for a private goal; for a group goal, a manager of that goal, checked
// live (managers are per goal, and the Owner recorded is the approver).
func (s *Service) checkDecider(ctx context.Context, d Domain, g Goal, decision ExecutionDecision) error {
	if !d.GroupOwned() {
		if decision.Owner != d.Owner {
			return ErrDenied
		}
		return nil
	}
	ga, ok := s.Auth.(GroupAuthorizer)
	if decision.Owner == "" || !ok {
		return ErrDenied
	}
	return ga.AuthorizeGoalManager(ctx, Actor{Domain: d, User: decision.Owner}, g)
}

// validate checks the terms against server time and the goal owner. A group
// goal is recognized by its owner being a member subject-set.
func (t ExecutionTerms) validate(now time.Time, owner string) error {
	groupGoal := Domain{Owner: owner}.GroupOwned()
	if t.ActionApproval != "" && t.ActionApproval != "manual" && t.ActionApproval != "standing_private" && t.ActionApproval != "standing_plan" {
		return fmt.Errorf("%w: unknown action approval policy", ErrInvalid)
	}
	if groupGoal {
		if t.ActionApproval == "standing_private" {
			return fmt.Errorf("%w: standing_private is not available on group goals; use standing_plan", ErrInvalid)
		}
		if t.Group == nil || t.Destination != (PrivateDestination{}) {
			return fmt.Errorf("%w: a group goal requires exactly one destination: group, not a private destination", ErrInvalid)
		}
		if t.Group.MemberGroup != owner {
			return fmt.Errorf("%w: group.memberGroup must equal the goal owner", ErrInvalid)
		}
		if t.Group.Channel == "" || t.Group.ChannelUID == "" {
			return fmt.Errorf("%w: group destination requires a pinned channel", ErrInvalid)
		}
		if t.ActionApproval == "standing_plan" {
			if t.StandingPlan == nil || len(t.StandingPlan.Phases) == 0 {
				return fmt.Errorf("%w: standing_plan requires a standing plan with at least one phase", ErrInvalid)
			}
			for _, ph := range t.StandingPlan.Phases {
				for _, sl := range ph.Slots {
					if sl.ID == "" {
						return fmt.Errorf("%w: standing plan slot of type %q must name a concrete id", ErrInvalid, sl.Type)
					}
				}
			}
		}
	} else {
		if t.Group != nil {
			return fmt.Errorf("%w: a private goal requires exactly one destination: a private destination, not a group", ErrInvalid)
		}
		if t.ActionApproval == "standing_plan" || t.StandingPlan != nil {
			return fmt.Errorf("%w: standing_plan is only available on group goals", ErrInvalid)
		}
	}
	maxSpan, maxName := 30*24*time.Hour, "30 days"
	if groupGoal {
		maxSpan, maxName = 90*24*time.Hour, "90 days"
	}
	if t.Event != nil {
		if t.Schedule != nil || len(t.ScheduleWindows) != 0 {
			return fmt.Errorf("%w: event watches and calendar schedules are exclusive", ErrInvalid)
		}
		if err := t.Event.subscription(t.DueAt, t.ExpiresAt).Validate(); err != nil {
			return fmt.Errorf("%w: invalid event watch: %v", ErrInvalid, err)
		}
	}
	if t.DueAt.Before(now) {
		return fmt.Errorf("%w: dueAt must be in the future; server time is %s; choose a later start with time for approval", ErrInvalid, now.UTC().Format(time.RFC3339))
	}
	if !t.ExpiresAt.After(t.DueAt) || t.ExpiresAt.Sub(now) > maxSpan {
		return fmt.Errorf("%w: expiresAt must be after dueAt and within %s of server time %s", ErrInvalid, maxName, now.UTC().Format(time.RFC3339))
	}
	permittedReport := false
	for _, op := range t.AllowedOperations {
		if op == "report_goal_event" {
			permittedReport = true
		}
	}
	if permittedReport != (t.Report != nil) {
		return fmt.Errorf("%w: observation reporting requires an exact report policy and operation", ErrInvalid)
	}
	if t.Report != nil {
		if err := t.Report.Validate(); err != nil {
			return err
		}
	}
	b := t.Bounds
	for _, limit := range []struct {
		name           string
		value, maximum int64
	}{
		{"durationSeconds", b.DurationSeconds, 86400}, {"turns", b.Turns, 10000}, {"tokens", b.Tokens, 10000000},
	} {
		if limit.value < 1 || limit.value > limit.maximum {
			return fmt.Errorf("%w: bounds.%s must be between 1 and %d (received %d); class limits may be lower", ErrInvalid, limit.name, limit.maximum, limit.value)
		}
	}
	if b.ApprovalSeconds < 1 || b.ApprovalSeconds > b.DurationSeconds {
		return fmt.Errorf("%w: bounds.approvalSeconds must be between 1 and bounds.durationSeconds=%d (received %d)", ErrInvalid, b.DurationSeconds, b.ApprovalSeconds)
	}
	if strings.TrimSpace(t.ClassDigest) == "" {
		return fmt.Errorf("%w: execution requires a pinned class", ErrInvalid)
	}
	if !groupGoal && (t.Destination.Channel == "" || t.Destination.ChannelUID == "" || t.Destination.Recipient != owner) {
		return fmt.Errorf("%w: execution requires a pinned class and private destination", ErrInvalid)
	}
	if len(t.AllowedOperations) < 1 || len(t.AllowedOperations) > 32 || len(t.Evidence) < 1 || len(t.Evidence) > 32 {
		return fmt.Errorf("%w: allowedOperations and evidence each require between 1 and 32 entries", ErrInvalid)
	}
	for _, values := range [][]string{t.AllowedOperations, t.Evidence} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 1024 {
				return ErrInvalid
			}
		}
	}
	return nil
}

func (s *Service) RequestExecution(ctx context.Context, a Actor, r ExecutionRequest) (Goal, error) {
	if err := s.authorize(ctx, a, true); err != nil {
		return Goal{}, err
	}
	if s.ExecutionAuth == nil {
		return Goal{}, ErrDenied
	}
	if !validRequestID(r.RequestID) || r.Revision < 1 || (r.ApprovalMode != "" && r.ApprovalMode != "plan") {
		return Goal{}, ErrInvalid
	}
	// The authority's PrepareExecution freezes an authored standing plan into
	// the terms and clears it; one still here was never frozen or checked.
	if len(r.StandingPlan) > 0 {
		return Goal{}, fmt.Errorf("%w: a standing plan is only for group goals", ErrInvalid)
	}
	// Do not accept caller-supplied windows. Resolution is independent of now,
	// preserving retry identity after the first due time has passed.
	r.Terms.ScheduleWindows = nil
	if r.Terms.Event != nil && r.Terms.Schedule != nil {
		return Goal{}, ErrInvalid
	}
	if r.Terms.Schedule != nil {
		windows, err := sessionschedule.Resolve(*r.Terms.Schedule, r.Terms.DueAt, r.Terms.ExpiresAt)
		if err != nil {
			return Goal{}, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		r.Terms.ScheduleWindows = windows
	}
	h, err := requestHash(r)
	if err != nil {
		return Goal{}, err
	}
	if prior, ok, err := s.Store.Receipt(ctx, a.Domain, r.RequestID, h); err != nil || ok {
		if err == nil {
			err = s.Auth.ReadGoal(ctx, a, prior)
		}
		return prior, err
	}
	g, err := s.get(ctx, a, r.ID)
	if err != nil {
		return Goal{}, err
	}
	prior, err := EncodePrior(g)
	if err != nil {
		return Goal{}, err
	}
	if g.Revision != r.Revision {
		return Goal{}, ErrConflict
	}
	if g.State != Active {
		return Goal{}, fmt.Errorf("%w: execution requires active goal", ErrInvalid)
	}
	if err := s.requireGroupManager(ctx, a, g, "request_execution"); err != nil {
		return Goal{}, err
	}
	sources, err := s.Auth.Sources(ctx, a)
	if err != nil {
		return Goal{}, err
	}
	g.Sources = mergeSources(g.Sources, sources)
	now := s.now()
	if err := r.Terms.validate(now, a.Domain.Owner); err != nil {
		return Goal{}, err
	}
	host := consentHost(a, g)
	g.Execution = &ExecutionConsent{Session: host.Session, SessionUID: host.SessionUID}
	if err := s.validateExecution(ctx, g, r.Terms, a.User); err != nil {
		return Goal{}, err
	}
	r.Terms.DueAt = r.Terms.DueAt.UTC()
	r.Terms.ExpiresAt = r.Terms.ExpiresAt.UTC()
	g.Revision++
	g.UpdatedAt = now
	// A request replaces the approval outright: a pending renewal of the old
	// one, and the approval a renewal replaced, go with it.
	g.Execution, g.PendingRenewal, g.PriorExecution = nil, nil, nil
	// Commit to the entire reviewed goal, dependencies, revision and terms.
	digest, err := requestHash(struct {
		Goal         Goal
		Session      string
		SessionUID   string
		Terms        ExecutionTerms
		ApprovalMode string
	}{g, host.Session, host.SessionUID, r.Terms, r.ApprovalMode})
	if err != nil {
		return Goal{}, err
	}
	g.Execution = &ExecutionConsent{ApprovalMode: r.ApprovalMode, Session: host.Session, SessionUID: host.SessionUID, RequestID: r.RequestID, RequestRevision: g.Revision, Digest: digest, Terms: r.Terms}
	if preflight, ok := s.ExecutionAuth.(interface {
		CheckExecutionConsent(context.Context, Goal) error
	}); ok {
		if err := preflight.CheckExecutionConsent(ctx, g); err != nil {
			return Goal{}, err
		}
	}
	return s.commit(ctx, a, g, r.Revision, prior, r.RequestID, h, "request_execution")
}

// CheckDecision answers, before a decision is recorded anywhere, whether
// decider may decide the pending request digest on goal id with approved. It
// is DecideExecution's own predicate — decider check, then the decide-time
// rules for an approval — so a decision it accepts is one DecideExecution
// will accept. The decider's own earlier, identical decision is accepted
// again (a retry); anyone else's is a conflict.
func (s *Service) CheckDecision(ctx context.Context, d Domain, id, digest, decider string, approved bool) error {
	if s.ExecutionAuth == nil || s.Auth == nil || s.validateDomain(d) != nil {
		return ErrDenied
	}
	g, err := s.Store.Get(ctx, d, id)
	if err != nil {
		return err
	}
	c, view, renewal := consentFor(g, digest)
	if c == nil {
		return fmt.Errorf("%w: this approval request is no longer pending", ErrConflict)
	}
	if c.Decision != nil {
		if c.Decision.Owner == decider && c.Decision.Approved == approved {
			return nil
		}
		return fmt.Errorf("%w: this request was already decided", ErrConflict)
	}
	if g.State != Active || g.Revision != pendingRevision(c, renewal) || !s.now().Before(c.Terms.ExpiresAt) {
		return fmt.Errorf("%w: the goal changed since this approval was requested", ErrConflict)
	}
	if err := s.checkDecider(ctx, d, g, ExecutionDecision{Owner: decider}); err != nil {
		return err
	}
	if approved {
		return s.validateExecution(ctx, view, c.Terms, decider)
	}
	return nil
}

// DecideExecution is an internal component path, not an agent tool. The signed
// witness is retained atomically with the decision and audit intent. A grant
// write may be retried independently: no dispatch is possible until both agree.
func (s *Service) DecideExecution(ctx context.Context, d Domain, id string, decision ExecutionDecision) (Goal, error) {
	if s.ExecutionAuth == nil || s.Auth == nil || s.validateDomain(d) != nil || !validRequestID(decision.RequestID) {
		return Goal{}, ErrDenied
	}
	g, err := s.Store.Get(ctx, d, id)
	if err != nil {
		return Goal{}, err
	}
	// The decision names either the pending request in Execution or a
	// group goal's pending renewal; view is the goal as that card showed it.
	c, view, renewal := consentFor(g, decision.Digest)
	if c == nil || decision.Witness == "" {
		return Goal{}, ErrDenied
	}
	if err := s.checkDecider(ctx, d, g, decision); err != nil {
		return Goal{}, err
	}
	if err := s.ExecutionAuth.VerifyDecision(ctx, view, decision); err != nil {
		return Goal{}, err
	}
	if c.Decision != nil {
		if *c.Decision == decision {
			return g, nil
		}
		return Goal{}, ErrConflict
	}
	if g.State != Active || g.Revision != pendingRevision(c, renewal) || !s.now().Before(c.Terms.ExpiresAt) {
		return Goal{}, ErrConflict
	}
	if decision.Approved {
		if err := s.validateExecution(ctx, view, c.Terms, decision.Owner); err != nil {
			return Goal{}, err
		}
	}
	h, err := requestHash(decision)
	if err != nil {
		return Goal{}, err
	}
	if renewal {
		return s.decideRenewal(ctx, g, c, decision, h)
	}
	prior, err := EncodePrior(g)
	if err != nil {
		return Goal{}, err
	}
	expected := g.Revision
	g.Revision++
	g.UpdatedAt = s.now()
	c.Decision = &decision
	if decision.Approved && d.GroupOwned() {
		terms := c.Terms
		g.LastApprovedTerms = &terms
	}
	// The witness is embedded in Execution.Decision, not substituted for the
	// opening actor envelope (which has a different signature/content shape).
	eventID, err := newID("goalev-")
	if err != nil {
		return Goal{}, err
	}
	accepted, err := s.Store.Commit(ctx, Mutation{Goal: g, Expected: expected, RequestID: decision.RequestID, Hash: h, Prior: prior,
		Event: Event{ID: eventID, Goal: g, Action: "execution_decision", Session: g.OriginSession, Proof: decision.Witness}})
	if err != nil {
		return Goal{}, err
	}
	// A concurrent receipt winner is checked against the same verified human
	// decision. No opening-message actor is invented for the component path.
	if accepted.Execution == nil || accepted.Execution.Decision == nil || *accepted.Execution.Decision != decision {
		return Goal{}, ErrConflict
	}
	if err := s.ExecutionAuth.VerifyDecision(ctx, accepted, decision); err != nil {
		return Goal{}, err
	}
	return accepted, nil
}

// decideRenewal records a manager's decision on g's pending renewal c, at
// the current revision: a renewal changes consent only, so nothing bound to
// the revision — runs in flight, runs already scheduled — is superseded.
//
// Approved, the renewal becomes the approval in force, and the approval it
// renews moves to PriorExecution, where it keeps governing the runs it
// scheduled until it runs out; queued runs the renewal also covers are
// rebound to it when it is scheduled (OccurrenceStore.Schedule). Declined,
// the decided renewal stays pending-and-decided so the same approval is not
// asked again, and the approval in force runs on to its expiry.
func (s *Service) decideRenewal(ctx context.Context, g Goal, c *ExecutionConsent, decision ExecutionDecision, h string) (Goal, error) {
	store, ok := s.Store.(InPlaceStore)
	if !ok {
		return Goal{}, ErrDenied
	}
	prior, err := EncodePrior(g)
	if err != nil {
		return Goal{}, err
	}
	next := g
	decided := *c
	decided.Decision = &decision
	action := "renewal_decision"
	if decision.Approved {
		next.PriorExecution, next.Execution, next.PendingRenewal = g.Execution, &decided, nil
		terms := decided.Terms
		next.LastApprovedTerms = &terms
		// The same action as a first approval, so the outbox schedules it.
		action = "execution_decision"
	} else {
		next.PendingRenewal = &decided
	}
	next.UpdatedAt = s.now()
	eventID, err := newID("goalev-inplace-")
	if err != nil {
		return Goal{}, err
	}
	accepted, err := store.CommitInPlace(ctx, Mutation{Goal: next, Expected: g.Revision, RequestID: decision.RequestID, Hash: h, Prior: prior,
		Event: Event{ID: eventID, Goal: next, Action: action, Session: g.OriginSession, Proof: decision.Witness}})
	if err != nil {
		return Goal{}, err
	}
	recorded := accepted.PendingRenewal
	if decision.Approved {
		recorded = accepted.Execution
	}
	if recorded == nil || recorded.Digest != decision.Digest || recorded.Decision == nil || *recorded.Decision != decision {
		return Goal{}, ErrConflict
	}
	if decision.Approved {
		if err := s.ExecutionAuth.VerifyDecision(ctx, accepted, decision); err != nil {
			return Goal{}, err
		}
	}
	return accepted, nil
}

func (s *Service) Dispatchable(ctx context.Context, g Goal) error {
	if s.ExecutionAuth == nil || s.Store == nil {
		return ErrDenied
	}
	if _, durable := s.Store.(OccurrenceStore); !durable {
		return ErrDenied
	}
	if g.Execution == nil {
		return ErrDenied
	}
	current, err := s.Store.Get(ctx, g.Domain, g.ID)
	if err != nil {
		return err
	}
	if current.Revision != g.Revision {
		return ErrDenied
	}
	// Re-read durable state: an old approved snapshot must not survive a pause,
	// edit, cancellation or replacement consent at activation time. The
	// consent is the one g names — the approval in force, or the one it
	// renewed, which still governs the runs it scheduled.
	g, ok := current.ForConsent(g.Execution.Digest)
	if !ok {
		return ErrDenied
	}
	c := g.Execution
	// A group goal's decision names the approving manager, not the group
	// owner; AcceptsDecider is the owner equality for a private goal.
	if g.State != Active || c == nil || c.Decision == nil || !c.Decision.Approved ||
		!g.Domain.AcceptsDecider(c.Decision.Owner) || c.Decision.Digest != c.Digest || g.Revision != c.RequestRevision+1 ||
		s.now().Before(c.Terms.DueAt) || !s.now().Before(c.Terms.ExpiresAt) {
		return ErrDenied
	}
	if err := s.ExecutionAuth.VerifyDecision(ctx, g, *c.Decision); err != nil {
		return err
	}
	if err := s.validateDispatch(ctx, g, c.Terms); err != nil {
		return err
	}
	return s.ExecutionAuth.AuthorizeDispatch(ctx, g)
}

// ExecutionWindows is the exact finite list reviewed by the human. Legacy
// one-time requests retain their original window and occurrence identity.
func (t ExecutionTerms) ExecutionWindows() ([]sessionschedule.Window, error) {
	if t.Event != nil {
		return nil, ErrDenied
	}
	if t.Schedule == nil {
		if len(t.ScheduleWindows) != 0 {
			return nil, ErrDenied
		}
		return []sessionschedule.Window{{DueAt: t.DueAt, ExpiresAt: t.ExpiresAt}}, nil
	}
	if len(t.ScheduleWindows) < 1 || len(t.ScheduleWindows) > sessionschedule.MaxRuns || len(t.ScheduleWindows) > t.Schedule.MaxRuns {
		return nil, ErrDenied
	}
	var last time.Time
	for _, w := range t.ScheduleWindows {
		if w.DueAt.Before(t.DueAt) || !w.ExpiresAt.After(w.DueAt) || w.ExpiresAt.After(t.ExpiresAt) || (!last.IsZero() && !w.DueAt.After(last)) {
			return nil, ErrDenied
		}
		last = w.DueAt
	}
	return t.ScheduleWindows, nil
}

func (t ExecutionTerms) ContainsWindow(due, expiry time.Time) bool {
	windows, err := t.ExecutionWindows()
	if err != nil {
		return false
	}
	for _, w := range windows {
		if w.DueAt.Equal(due) && w.ExpiresAt.Equal(expiry) {
			return true
		}
	}
	return false
}

// DispatchOccurrence binds activation and every governed action to an exact
// approved window, rather than the enclosing series authorization period.
func (s *Service) DispatchOccurrence(ctx context.Context, g Goal, o Occurrence) error {
	if s.Store == nil {
		return ErrDenied
	}
	current, err := s.Store.Get(ctx, g.Domain, g.ID)
	if err != nil {
		return err
	}
	if current.Revision != g.Revision {
		return ErrDenied
	}
	// Window membership must come from durable consent, never a caller
	// snapshot: the consent this occurrence is bound to.
	g, bound := current.ForConsent(o.ConsentDigest)

	if !bound || g.Domain != o.Domain || g.ID != o.GoalID || g.Revision != o.GoalRevision ||
		s.now().Before(o.DueAt) || !s.now().Before(o.ExpiresAt) {
		return ErrDenied
	}
	if g.Execution.Terms.Event != nil {
		if s.Events == nil {
			return ErrDenied
		}
		if err := s.Events.CheckOccurrence(ctx, g, o); err != nil {
			return err
		}
	} else if o.Event != nil || !g.Execution.Terms.ContainsWindow(o.DueAt, o.ExpiresAt) {
		return ErrDenied
	}
	return s.Dispatchable(ctx, g)
}
