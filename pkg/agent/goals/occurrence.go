package goals

import (
	"context"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

type OccurrenceState string

const (
	OccurrenceQueued  OccurrenceState = "queued"
	OccurrenceSkipped OccurrenceState = "skipped"
	OccurrenceClaimed OccurrenceState = "claimed"
	OccurrenceRunning OccurrenceState = "running"
	// Retained sessions have verified terminal runners and no execution authority.
	// They remain inspectable until cleanup without reserving execution capacity.
	OccurrenceRetained  OccurrenceState = "retained"
	OccurrenceUnknown   OccurrenceState = "unknown"
	OccurrenceFinished  OccurrenceState = "finished"
	OccurrenceSucceeded OccurrenceState = "succeeded"
	OccurrenceFailed    OccurrenceState = "failed"
	OccurrenceCancelled OccurrenceState = "cancelled"
)

// Occurrence persists the session name before any create attempt. Lease expiry
// only transfers worker ownership; it never forgets or replaces that session.
type Occurrence struct {
	Event         *sessionevents.Launch `json:"event,omitempty"`
	ID            string                `json:"id"`
	Domain        Domain                `json:"domain"`
	GoalID        string                `json:"goalID"`
	GoalRevision  int64                 `json:"goalRevision"`
	ConsentDigest string                `json:"consentDigest"`
	DueAt         time.Time             `json:"dueAt"`
	ExpiresAt     time.Time             `json:"expiresAt"`
	State         OccurrenceState       `json:"state"`
	Worker        string                `json:"worker,omitempty"`
	Fence         int64                 `json:"fence"`
	LeaseUntil    time.Time             `json:"leaseUntil,omitempty"`
	SessionName   string                `json:"sessionName"`
	SessionUID    string                `json:"sessionUID,omitempty"`
	Proposal      *RunProposal          `json:"proposal,omitempty"`
	Outcome       *RunOutcome           `json:"outcome,omitempty"`
	Cost          *RunCost              `json:"cost,omitempty"`
	Reply         *RunReply             `json:"reply,omitempty"`
}

type ClaimRequest struct {
	ID         string
	Worker     string
	Now        time.Time
	Lease      time.Duration
	OwnerLimit int
	ClassLimit int
}

// OccurrenceStore is a durable-only dispatch ledger, separate from the goal
// management Store. Callers must pass Dispatchable before Schedule and Claim,
// and recheck authority at activation. Store transactions also compare current
// goal revision so management changes cannot race a claim into eligibility.
type OccurrenceStore interface {
	Schedule(context.Context, Goal) (Occurrence, error)
	Due(context.Context, time.Time, int) ([]Occurrence, error)
	Occurrence(context.Context, string) (Occurrence, error)
	Claim(context.Context, ClaimRequest) (Occurrence, error)
	Attach(context.Context, Occurrence, string, time.Time) (Occurrence, error)
	Renew(context.Context, Occurrence, time.Time, time.Duration) (Occurrence, error)
	// Finish requires authoritative terminal acknowledgement. Unknown keeps
	// the reservation until reconciliation determines whether effects occurred.
	Finish(context.Context, Occurrence, OccurrenceState, time.Time) (Occurrence, error)
}

// QueuedSweeper records work that can no longer launch without reserving
// capacity or pretending a session existed. The transaction must serialize with Claim.
type QueuedSweeper interface {
	SweepQueued(context.Context, time.Time, int) (int, error)
}

// RunOutcome records operator observations, not verification of the goal's
// outcome or transport delivery. Session termination alone proves neither.
type RunOutcome struct {
	Reason     RunReason `json:"reason"`
	ObservedAt time.Time `json:"observedAt"`
	Effects    string    `json:"effects"`
}

type RunReason string

const (
	RunSessionEnded         RunReason = "session_ended"
	RunSessionFailed        RunReason = "session_failed"
	RunInfrastructureFailed RunReason = "infrastructure_failed"
	RunDurationExpired      RunReason = "duration_expired"
	RunConsentExpired       RunReason = "consent_expired"
	RunMissed               RunReason = "missed_window"
	RunCancelled            RunReason = "cancelled"
	RunPaused               RunReason = "paused"
	RunSuperseded           RunReason = "superseded"
	RunAuthorityDenied      RunReason = "authority_denied"
	RunSessionMissing       RunReason = "session_missing"
)

func (r RunReason) Valid() bool {
	switch r {
	case RunMissed, RunSessionEnded, RunSessionFailed, RunInfrastructureFailed, RunDurationExpired, RunConsentExpired, RunCancelled, RunPaused, RunSuperseded, RunAuthorityDenied, RunSessionMissing:
		return true
	}
	return false
}

// RunStore retains the first observed stop reason before session cleanup.
// Controller observations require the current dispatch fence and exact session
// UID. Result proposals bind the authenticated root UID and revision, using
// the transaction's current fence independently of the worker lease. Capacity
// is released separately, only after termination acknowledgement.
type RunStore interface {
	RecordOutcome(context.Context, Occurrence, RunReason, time.Time) (Occurrence, error)
	Runs(context.Context, Domain, string, ListRequest) (RunPage, error)
	ProposeResult(context.Context, Occurrence, RunProposal, time.Time) (Occurrence, error)
}

type RunPage struct {
	Runs []Occurrence `json:"runs"`
	Next string       `json:"next,omitempty"`
}

func (s *Service) Runs(ctx context.Context, a Actor, id string, r ListRequest) (RunPage, error) {
	if _, err := s.Get(ctx, a, id); err != nil {
		return RunPage{}, err
	}
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Limit < 1 || r.Limit > 100 || len(r.After) > 128 || r.State != "" {
		return RunPage{}, ErrInvalid
	}
	store, ok := s.Store.(RunStore)
	if !ok {
		return RunPage{}, ErrDenied
	}
	page, err := store.Runs(ctx, a.Domain, id, r)
	if err != nil {
		return RunPage{}, err
	}
	for _, run := range page.Runs {
		if run.Event != nil {
			if s.Events == nil || s.Events.Sources == nil {
				return RunPage{}, ErrDenied
			}
			if err := s.Events.Sources.Check(ctx, a.Domain.Owner, run.Event.Observation.Source, run.Event.Observation.Dependencies); err != nil {
				return RunPage{}, err
			}
		}
		if run.Proposal != nil || run.Reply != nil || run.Event != nil {
			if err := s.Auth.ReadGoal(ctx, a, Goal{Sources: run.ReadDependencies()}); err != nil {
				return RunPage{}, err
			}
		}
	}
	return page, nil
}

// RunProposal is an agent's account, never a transport receipt or authorization
// to complete the goal. The authenticated root is bound by the controller; the
// caller cannot choose its occurrence, owner, revision, fence or session UID.
type RunProposal struct {
	Observation *ObservationReport `json:"observation,omitempty"`
	RequestID   string             `json:"requestID"`
	Status      string             `json:"status"`
	Summary     string             `json:"summary"`
	Evidence    []string           `json:"evidence"`
	Sources     []Source           `json:"sources,omitempty"`
	SubmittedAt time.Time          `json:"submittedAt"`
}

func (p RunProposal) Validate() error {
	if p.Observation != nil {
		if p.Status != "reported_success" {
			return ErrInvalid
		}
		if err := p.Observation.Validate(); err != nil {
			return err
		}
	}
	if !validRequestID(p.RequestID) || strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 4000 || len(p.Evidence) > 32 {
		return ErrInvalid
	}
	switch p.Status {
	case "reported_success", "blocked", "failed", "unknown":
	default:
		return ErrInvalid
	}
	if p.Status == "reported_success" && len(p.Evidence) == 0 {
		return ErrInvalid
	}
	for _, ref := range p.Evidence {
		if strings.TrimSpace(ref) == "" || len(ref) > 512 {
			return ErrInvalid
		}
	}
	return nil
}

// ReadDependencies preserves external source gates while retaining the run's
// own private conversation under its durable goal domain. Its authenticated
// root name/UID were bound by the operator; deleting ephemeral agentsession
// relationships must not make the owner's stored result unreadable. This is
// never applied to another session's dependencies.
func (o Occurrence) ReadDependencies() []Source {
	var sources []Source
	if o.Event != nil {
		for _, dep := range o.Event.Observation.Dependencies {
			sources = append(sources, Source{ResourceType: dep.ResourceType, ResourceID: dep.ResourceID, Permission: dep.Permission})
		}
	}
	if o.Proposal != nil {
		sources = append(sources, o.Proposal.Sources...)
	}
	if o.Reply != nil {
		sources = append(sources, o.Reply.Sources...)
	}
	deps := make([]Source, 0, len(sources))
	for _, src := range sources {
		if o.SessionUID != "" && src.ResourceType == "agentsession" && src.ResourceID == o.Domain.Namespace+"/"+o.SessionName {
			src = Source{ResourceType: "agent_goal_domain", ResourceID: o.Domain.ID(), Permission: "view_memory"}
		}
		deps = mergeSources(deps, []Source{src})
	}
	return deps
}
