package goals

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

// DiscoveryRequest asks for separately reviewed permission to suggest private goals.
// It cannot execute a goal. Each proposed monitor needs its own exact consent.
type DiscoveryRequest struct {
	RequestID       string                  `json:"requestID"`
	Title           string                  `json:"title"`
	Outcome         string                  `json:"outcome"`
	Terms           ExecutionTerms          `json:"terms"`
	Predicate       sessionevents.Predicate `json:"predicate"`
	SubjectField    string                  `json:"subjectField"`
	MaxProposals    int                     `json:"maxProposals"`
	MaxPending      int                     `json:"maxPending"`
	ProposalSeconds int64                   `json:"proposalSeconds"`
}

type DiscoveryPolicy struct {
	ID       string             `json:"id"`
	Actor    Actor              `json:"actor"`
	Request  DiscoveryRequest   `json:"request"`
	Template Goal               `json:"template"`
	Decision *ExecutionDecision `json:"decision,omitempty"`
	Stopped  bool               `json:"stopped"`
}

type DiscoveryReference struct {
	PolicyID       string `json:"policyID"`
	ProposalID     string `json:"proposalID"`
	EvidenceDigest string `json:"evidenceDigest"`
}

type DiscoveryProposal struct {
	Revision    int64                     `json:"revision"`
	ID          string                    `json:"id"`
	PolicyID    string                    `json:"policyID"`
	Goal        Goal                      `json:"goal"`
	Observation sessionevents.Observation `json:"observation"`
	CreatedAt   time.Time                 `json:"createdAt"`
	ExpiresAt   time.Time                 `json:"expiresAt"`
	State       string                    `json:"state"`
	Decision    *ExecutionDecision        `json:"decision,omitempty"`
}

func (r DiscoveryRequest) Validate(now time.Time, owner string) error {
	if !validRequestID(r.RequestID) || r.Terms.Event == nil || r.Terms.Schedule != nil || r.Predicate.Validate() != nil || strings.TrimSpace(r.SubjectField) == "" || len(r.SubjectField) > 128 || r.MaxProposals < 1 || r.MaxProposals > 100 || r.MaxPending < 1 || r.MaxPending > r.MaxProposals || r.ProposalSeconds < 1 || r.ProposalSeconds > 86400 {
		return ErrInvalid
	}
	if err := r.Terms.validate(now, owner); err != nil {
		return err
	}
	return validateGoal(Goal{Domain: Domain{"validation", "validation", "validation", "validation"}, Title: r.Title, Outcome: r.Outcome, State: Draft})
}

func DiscoverySubscription(p DiscoveryPolicy) (sessionevents.Subscription, error) {
	if p.Template.Execution == nil || p.Template.Execution.Terms.Event == nil {
		return sessionevents.Subscription{}, ErrDenied
	}
	c := p.Template.Execution
	start := c.Terms.DueAt
	if p.Decision != nil {
		var entry struct {
			CreatedAt time.Time `json:"createdAt"`
		}
		if err := json.Unmarshal([]byte(p.Decision.Witness), &entry); err != nil || entry.CreatedAt.IsZero() {
			return sessionevents.Subscription{}, ErrDenied
		}
		if entry.CreatedAt.After(start) {
			start = entry.CreatedAt
		}
	}
	sub := c.Terms.Event.subscription(start, c.Terms.ExpiresAt)
	sub.Consumer = "goal-discovery"
	sub.ID = "discoverywatch-" + c.Digest
	sub.Principal = p.Template.Domain.Owner
	raw, err := json.Marshal(eventTarget{p.Template.Domain, p.ID})
	if err != nil {
		return sub, err
	}
	sub.Target = string(raw)
	sub.Authority = c.Digest
	sub.Predicate = p.Request.Predicate
	sub.MaxRuns = p.Request.MaxProposals
	return sub, sub.Validate()
}

func NewDiscoveryProposal(p DiscoveryPolicy, l sessionevents.Launch, now time.Time) (DiscoveryProposal, error) {
	expected, err := DiscoverySubscription(p)
	if err != nil || !reflect.DeepEqual(expected, l.Subscription) || l.Observation.Validate() != nil || l.Observation.Source != expected.Source || !expected.Predicate.Matches(l.Observation) || l.Admission.Disposition != "accepted" || l.Admission.SubscriptionID != expected.ID || l.Admission.ObservationID != l.Observation.ID() || l.Admission.LaunchID != sessionevents.LaunchID(expected.ID, l.Observation.ID()) || now.Before(l.Admission.Window.DueAt) || !now.Before(l.Admission.Window.ExpiresAt) {
		return DiscoveryProposal{}, ErrDenied
	}
	if len(l.Observation.Data) > 8192 {
		return DiscoveryProposal{}, fmt.Errorf("%w: discovery evidence too large", ErrInvalid)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(l.Observation.Data, &fields); err != nil {
		return DiscoveryProposal{}, ErrInvalid
	}
	var subject string
	if err := json.Unmarshal(fields[p.Request.SubjectField], &subject); err != nil || strings.TrimSpace(subject) == "" || len(subject) > 256 {
		return DiscoveryProposal{}, ErrInvalid
	}
	id := "discovery-" + strings.TrimPrefix(l.Admission.LaunchID, "evlaunch-")
	g := p.Template
	// Clone before specializing so no pointer in the policy is changed.
	raw, err := json.Marshal(g)
	if err != nil {
		return DiscoveryProposal{}, err
	}
	g = Goal{}
	if err = json.Unmarshal(raw, &g); err != nil {
		return DiscoveryProposal{}, err
	}
	g.ID = "goal-" + strings.TrimPrefix(id, "discovery-")
	g.Revision = 1
	g.State = Active
	g.CreatedAt = now
	g.UpdatedAt = now
	for _, dep := range l.Observation.Dependencies {
		g.Sources = mergeSources(g.Sources, []Source{{dep.ResourceType, dep.ResourceID, dep.Permission}})
	}
	evidence, err := l.Observation.Digest()
	if err != nil {
		return DiscoveryProposal{}, err
	}
	g.Discovery = &DiscoveryReference{PolicyID: p.ID, ProposalID: id, EvidenceDigest: evidence}
	g.Execution.Terms.Event.Predicate.Subject = subject
	g.Execution.Terms.DueAt = now.UTC()
	g.Execution.Purpose = "discovery_proposal"
	g.Execution.RequestID = id
	g.Execution.RequestRevision = 1
	g.Execution.Decision = nil
	g.Execution.Digest = ""
	digest, err := requestHash(g)
	if err != nil {
		return DiscoveryProposal{}, err
	}
	g.Execution.Digest = digest
	expiry := now.Add(time.Duration(p.Request.ProposalSeconds) * time.Second)
	if expiry.After(g.Execution.Terms.ExpiresAt) {
		expiry = g.Execution.Terms.ExpiresAt
	}
	if !expiry.After(now) {
		return DiscoveryProposal{}, fmt.Errorf("%w: proposal expired", ErrDenied)
	}
	return DiscoveryProposal{Revision: 1, ID: id, PolicyID: p.ID, Goal: g, Observation: l.Observation, CreatedAt: now.UTC(), ExpiresAt: expiry.UTC(), State: "pending"}, nil
}

// DiscoveryStore commits suppression and proposal allowance atomically. Accepted
// decisions create the goal, exact execution consent and audit outbox together.
type DiscoveryStore interface {
	CreateDiscoveryPolicy(context.Context, DiscoveryPolicy) (DiscoveryPolicy, error)
	DiscoveryPolicy(context.Context, Domain, string) (DiscoveryPolicy, error)
	DecideDiscoveryPolicy(context.Context, DiscoveryPolicy, ExecutionDecision) (DiscoveryPolicy, error)
	StopDiscoveryPolicy(context.Context, Domain, string) error
	CreateDiscoveryProposal(context.Context, DiscoveryPolicy, DiscoveryProposal) (DiscoveryProposal, error)
	DiscoveryProposal(context.Context, Domain, string) (DiscoveryProposal, error)
	DecideDiscoveryProposal(context.Context, DiscoveryProposal, ExecutionDecision, time.Time) (DiscoveryProposal, error)
	DiscoveryNotified(context.Context, string) error
	DiscoveryPolicyNotification(context.Context, string) (bool, error)
	DiscoveryPolicyNotified(context.Context, string) error
	ActiveDiscoveryPolicies(context.Context, string, int) ([]DiscoveryPolicy, error)
	PendingDiscovery(context.Context, time.Time, int) ([]DiscoveryProposal, error)
}
