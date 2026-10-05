package goals

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
)

const ReportSourceKind = "goal-report"

type ReportSourceStore interface {
	SourceGoal(context.Context, string, string, string) (Goal, error)
}

// ReportSource reads immutable authenticated run proposals from the platform
// ledger. Ingress can only reference a record; it cannot supply event data.
// Individual runner cleanup does not change this source's incarnation.
type ReportSource struct {
	Store     Store
	Runs      OccurrenceStore
	Auth      Authorizer
	Execution ExecutionAuthority
}

type ReportReference struct {
	Source       sessionevents.Source `json:"source"`
	OccurrenceID string               `json:"occurrenceID"`
}

// ReportStream pins the goal's incarnation while allowing its sessions to be
// cleaned up. Recreating the goal starts a new stream even if its ID is reused.
func ReportStream(g Goal) sessionevents.Source {
	return sessionevents.Source{
		Kind:      ReportSourceKind,
		Namespace: g.Domain.Namespace,
		ID:        g.Domain.ID() + "/" + g.ID,
		UID: mustDigestReport(struct {
			Domain    Domain
			CreatedAt string
		}{g.Domain, g.CreatedAt.UTC().Format(time.RFC3339Nano)}),
	}
}

// Only JSON-encodable platform records are passed here; encoding failure is a
// programming error rather than a malformed ingress request.
func mustDigestReport(v any) string {
	d, err := requestHash(v)
	if err != nil {
		panic(err)
	}
	return d
}

func (*ReportSource) Kind() string { return ReportSourceKind }
func (a *ReportSource) goal(ctx context.Context, source sessionevents.Source) (Goal, error) {
	if a == nil {
		return Goal{}, sessionevents.ErrDenied
	}
	store, ok := a.Store.(ReportSourceStore)
	domain, id, valid := strings.Cut(source.ID, "/")
	if !ok || !valid || source.Kind != ReportSourceKind || source.Namespace == "" {
		return Goal{}, sessionevents.ErrDenied
	}
	g, err := store.SourceGoal(ctx, source.Namespace, domain, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Goal{}, sessionevents.ErrNotFound
		}
		if errors.Is(err, ErrDenied) {
			return Goal{}, sessionevents.ErrDenied
		}
		return Goal{}, err
	}
	expected := ReportStream(g)
	if source.UID != "" && source != expected {
		return Goal{}, sessionevents.ErrDenied
	}
	return g, nil
}

func (a *ReportSource) Resolve(ctx context.Context, principal string, source sessionevents.Source) (sessionevents.Source, error) {
	g, err := a.goal(ctx, source)
	if err != nil {
		return source, err
	}
	source = ReportStream(g)
	return source, a.Check(ctx, principal, source, nil)
}

func (a *ReportSource) Check(ctx context.Context, principal string, source sessionevents.Source, deps []sessionevents.Dependency) error {
	if a == nil || a.Auth == nil || source.UID == "" {
		return sessionevents.ErrDenied
	}
	g, err := a.goal(ctx, source)
	if err != nil {
		return err
	}
	g.Sources = mergeSources(g.Sources, []Source{{ResourceType: "agent_goal_domain", ResourceID: g.Domain.ID(), Permission: "view_memory"}})
	if principal != g.Domain.Owner {
		return sessionevents.ErrDenied
	}
	for _, dep := range deps {
		g.Sources = mergeSources(g.Sources, []Source{{ResourceType: dep.ResourceType, ResourceID: dep.ResourceID, Permission: dep.Permission}})
	}
	if err = a.Auth.ReadGoal(ctx, Actor{Domain: g.Domain}, g); err != nil {
		if errors.Is(err, ErrDenied) {
			return errors.Join(sessionevents.ErrDenied, err)
		}
		return err
	}
	return nil
}

func (a *ReportSource) Dependencies(ctx context.Context, principal string, source sessionevents.Source) ([]sessionevents.Dependency, error) {
	g, err := a.goal(ctx, source)
	if err != nil {
		return nil, err
	}
	deps := []sessionevents.Dependency{{ResourceType: "agent_goal_domain", ResourceID: g.Domain.ID(), Permission: "view_memory"}}
	for _, s := range g.Sources {
		deps = reportDependencies(deps, sessionevents.Dependency{ResourceType: s.ResourceType, ResourceID: s.ResourceID, Permission: s.Permission})
	}
	return deps, a.Check(ctx, principal, source, deps)
}

func (a *ReportSource) Verify(ctx context.Context, raw json.RawMessage) (sessionevents.Input, error) {
	var ref ReportReference
	if a == nil || a.Runs == nil || a.Execution == nil {
		return sessionevents.Input{}, sessionevents.ErrDenied
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		return sessionevents.Input{}, sessionevents.ErrInvalid
	}
	g, err := a.goal(ctx, ref.Source)
	if err != nil {
		return sessionevents.Input{}, err
	}
	o, err := a.Runs.Occurrence(ctx, ref.OccurrenceID)
	if err != nil {
		return sessionevents.Input{}, err
	}
	permitted := false
	if g.Execution != nil {
		for _, op := range g.Execution.Terms.AllowedOperations {
			if op == "report_goal_event" {
				permitted = true
			}
		}
	}
	if !permitted || o.Domain != g.Domain || o.GoalID != g.ID || o.SessionUID == "" ||
		o.Proposal == nil || o.Proposal.Observation == nil || o.Proposal.Status != "reported_success" ||
		g.Execution == nil || g.Revision != o.GoalRevision || g.Execution.Digest != o.ConsentDigest || g.Execution.Terms.Report == nil {
		return sessionevents.Input{}, sessionevents.ErrDenied
	}
	report := o.Proposal.Observation
	policy := g.Execution.Terms.Report
	if report.Kind != policy.Kind || report.Subject != policy.Subject || report.Validate() != nil {
		return sessionevents.Input{}, sessionevents.ErrDenied
	}
	if err = a.Execution.AuthorizeDispatch(ctx, g); err != nil {
		return sessionevents.Input{}, err
	}
	deps := ReportedDependencies(g, o)
	source := ReportStream(g)
	if err = a.Check(ctx, g.Domain.Owner, source, deps); err != nil {
		return sessionevents.Input{}, err
	}
	observation := sessionevents.Observation{
		Source:       source,
		EventID:      o.ID,
		Kind:         report.Kind,
		Subject:      report.Subject,
		ObservedAt:   o.Proposal.SubmittedAt,
		Data:         report.Data,
		Dependencies: deps,
		Witness: &sessionevents.Witness{
			Kind:      "agent-reported-goal-result",
			Reference: g.Domain.ID() + "/" + g.ID + "/" + o.ID,
			Digest:    mustDigestReport(o.Proposal),
		},
	}
	input := sessionevents.Input{Observation: observation, Publisher: "goal-run:" + o.ID, Sequence: 1}
	return input, input.Validate()
}

func reportDependencies(deps []sessionevents.Dependency, next sessionevents.Dependency) []sessionevents.Dependency {
	for _, existing := range deps {
		if existing == next {
			return deps
		}
	}
	return append(deps, next)
}

// ReportedDependencies carries both durable goal sources and everything read
// during this occurrence. No publication may trim dependencies to fit a limit.
func ReportedDependencies(g Goal, o Occurrence) []sessionevents.Dependency {
	deps := []sessionevents.Dependency{{ResourceType: "agent_goal_domain", ResourceID: g.Domain.ID(), Permission: "view_memory"}}
	for _, src := range mergeSources(g.Sources, o.ReadDependencies()) {
		deps = reportDependencies(deps, sessionevents.Dependency{ResourceType: src.ResourceType, ResourceID: src.ResourceID, Permission: src.Permission})
	}
	return deps
}
