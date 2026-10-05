package goals

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
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

type ExecutionTerms struct {
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
}

type ExecutionRequest struct {
	ApprovalMode string         `json:"approvalMode,omitempty"`
	RequestID    string         `json:"requestID"`
	ID           string         `json:"id"`
	Revision     int64          `json:"revision"`
	Terms        ExecutionTerms `json:"terms"`
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

func (t ExecutionTerms) validate(now time.Time, owner string) error {
	if t.ActionApproval != "" && t.ActionApproval != "manual" && t.ActionApproval != "standing_private" {
		return fmt.Errorf("%w: unknown action approval policy", ErrInvalid)
	}
	if t.Event != nil {
		if t.Schedule != nil || len(t.ScheduleWindows) != 0 {
			return fmt.Errorf("%w: event watches and calendar schedules are exclusive", ErrInvalid)
		}
		if err := t.Event.subscription(t.DueAt, t.ExpiresAt).Validate(); err != nil {
			return fmt.Errorf("%w: invalid event watch", ErrInvalid)
		}
	}
	b := t.Bounds
	if strings.TrimSpace(t.ClassDigest) == "" || t.DueAt.Before(now) || !t.ExpiresAt.After(t.DueAt) || t.ExpiresAt.Sub(now) > 30*24*time.Hour ||
		b.DurationSeconds < 1 || b.DurationSeconds > 86400 || b.Turns < 1 || b.Turns > 10000 || b.Tokens < 1 || b.Tokens > 10000000 ||
		b.ApprovalSeconds < 1 || b.ApprovalSeconds > b.DurationSeconds ||
		t.Destination.Channel == "" || t.Destination.ChannelUID == "" || t.Destination.Recipient != owner ||
		len(t.AllowedOperations) < 1 || len(t.AllowedOperations) > 32 || len(t.Evidence) < 1 || len(t.Evidence) > 32 {
		return fmt.Errorf("%w: execution requires a finite schedule, bounds, evidence and private destination", ErrInvalid)
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
	g, err := s.Get(ctx, a, r.ID)
	if err != nil {
		return Goal{}, err
	}
	if g.Revision != r.Revision {
		return Goal{}, ErrConflict
	}
	if g.State != Active {
		return Goal{}, fmt.Errorf("%w: execution requires active goal", ErrInvalid)
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
	g.Execution = &ExecutionConsent{Session: a.Session, SessionUID: a.SessionUID}
	if err := s.ExecutionAuth.Validate(ctx, g, r.Terms); err != nil {
		return Goal{}, err
	}
	r.Terms.DueAt = r.Terms.DueAt.UTC()
	r.Terms.ExpiresAt = r.Terms.ExpiresAt.UTC()
	g.Revision++
	g.UpdatedAt = now
	g.Execution = nil
	// Commit to the entire reviewed goal, dependencies, revision and terms.
	digest, err := requestHash(struct {
		Goal         Goal
		Session      string
		SessionUID   string
		Terms        ExecutionTerms
		ApprovalMode string
	}{g, a.Session, a.SessionUID, r.Terms, r.ApprovalMode})
	if err != nil {
		return Goal{}, err
	}
	g.Execution = &ExecutionConsent{ApprovalMode: r.ApprovalMode, Session: a.Session, SessionUID: a.SessionUID, RequestID: r.RequestID, RequestRevision: g.Revision, Digest: digest, Terms: r.Terms}
	if preflight, ok := s.ExecutionAuth.(interface {
		CheckExecutionConsent(context.Context, Goal) error
	}); ok {
		if err := preflight.CheckExecutionConsent(ctx, g); err != nil {
			return Goal{}, err
		}
	}
	return s.commit(ctx, a, g, r.Revision, r.RequestID, h, "request_execution")
}

// DecideExecution is an internal component path, not an agent tool. The signed
// witness is retained atomically with the decision and audit intent. A grant
// write may be retried independently: no dispatch is possible until both agree.
func (s *Service) DecideExecution(ctx context.Context, d Domain, id string, decision ExecutionDecision) (Goal, error) {
	if s.ExecutionAuth == nil || s.Auth == nil || d.Validate() != nil || !validRequestID(decision.RequestID) {
		return Goal{}, ErrDenied
	}
	g, err := s.Store.Get(ctx, d, id)
	if err != nil {
		return Goal{}, err
	}
	c := g.Execution
	if c == nil || c.Digest != decision.Digest || decision.Owner != d.Owner || decision.Witness == "" {
		return Goal{}, ErrDenied
	}
	if err := s.ExecutionAuth.VerifyDecision(ctx, g, decision); err != nil {
		return Goal{}, err
	}
	if c.Decision != nil {
		if *c.Decision == decision {
			return g, nil
		}
		return Goal{}, ErrConflict
	}
	if g.State != Active || g.Revision != c.RequestRevision || !s.now().Before(c.Terms.ExpiresAt) {
		return Goal{}, ErrConflict
	}
	if decision.Approved {
		if err := s.ExecutionAuth.Validate(ctx, g, c.Terms); err != nil {
			return Goal{}, err
		}
	}
	h, err := requestHash(decision)
	if err != nil {
		return Goal{}, err
	}
	expected := g.Revision
	g.Revision++
	g.UpdatedAt = s.now()
	c.Decision = &decision
	// The witness is embedded in Execution.Decision, not substituted for the
	// opening actor envelope (which has a different signature/content shape).
	eventID, err := newID("goalev-")
	if err != nil {
		return Goal{}, err
	}
	accepted, err := s.Store.Commit(ctx, Mutation{Goal: g, Expected: expected, RequestID: decision.RequestID, Hash: h,
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

func (s *Service) Dispatchable(ctx context.Context, g Goal) error {
	if s.ExecutionAuth == nil || s.Store == nil {
		return ErrDenied
	}
	if _, durable := s.Store.(OccurrenceStore); !durable {
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
	// edit, cancellation or replacement consent at activation time.
	g = current
	c := g.Execution
	if g.State != Active || c == nil || c.Decision == nil || !c.Decision.Approved ||
		c.Decision.Owner != g.Domain.Owner || c.Decision.Digest != c.Digest || g.Revision != c.RequestRevision+1 ||
		s.now().Before(c.Terms.DueAt) || !s.now().Before(c.Terms.ExpiresAt) {
		return ErrDenied
	}
	if err := s.ExecutionAuth.VerifyDecision(ctx, g, *c.Decision); err != nil {
		return err
	}
	if err := s.ExecutionAuth.Validate(ctx, g, c.Terms); err != nil {
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
	// Window membership must come from durable consent, never a caller snapshot.
	g = current

	if g.Domain != o.Domain || g.ID != o.GoalID || g.Revision != o.GoalRevision || g.Execution == nil ||
		g.Execution.Digest != o.ConsentDigest ||
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
