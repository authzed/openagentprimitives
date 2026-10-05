package goals

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionevents"
	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
)

// EventWatch is the finite event scope reviewed with execution consent. Consumer
// identity and authority are derived below, never supplied by a model.
type EventWatch struct {
	Source           sessionevents.Source         `json:"source"`
	Predicate        sessionevents.Predicate      `json:"predicate"`
	MaxRuns          int                          `json:"maxRuns"`
	RunWindowSeconds int64                        `json:"runWindowSeconds"`
	Timezone         string                       `json:"timezone"`
	QuietHours       []sessionschedule.QuietHours `json:"quietHours,omitempty"`
	Burst            string                       `json:"burst"`
}

func (w EventWatch) subscription(start, end time.Time) sessionevents.Subscription {
	return sessionevents.Subscription{ID: "validation", Principal: "validation", Target: "validation", Authority: "validation", Source: w.Source, Predicate: w.Predicate, StartsAt: start.UTC(), EndsAt: end.UTC(), MaxRuns: w.MaxRuns, RunWindowSeconds: w.RunWindowSeconds, Timezone: w.Timezone, QuietHours: w.QuietHours, Burst: w.Burst}
}

type eventTarget struct {
	Domain Domain `json:"domain"`
	GoalID string `json:"goalID"`
}

func EventSubscription(g Goal) (sessionevents.Subscription, error) {
	if g.Execution == nil || g.Execution.Terms.Event == nil {
		return sessionevents.Subscription{}, ErrDenied
	}
	start := g.Execution.Terms.DueAt
	if g.Execution.Purpose == "discovery_proposal" && g.Execution.Decision != nil {
		var witness struct {
			CreatedAt time.Time `json:"createdAt"`
		}
		if err := json.Unmarshal([]byte(g.Execution.Decision.Witness), &witness); err != nil || witness.CreatedAt.IsZero() {
			return sessionevents.Subscription{}, ErrDenied
		}
		if witness.CreatedAt.After(start) {
			start = witness.CreatedAt
		}
	}
	w := g.Execution.Terms.Event.subscription(start, g.Execution.Terms.ExpiresAt)
	target, err := json.Marshal(eventTarget{g.Domain, g.ID})
	if err != nil {
		return w, err
	}
	id, err := requestHash(struct {
		Domain   Domain
		ID       string
		Revision int64
		Digest   string
	}{g.Domain, g.ID, g.Revision, g.Execution.Digest})
	if err != nil {
		return w, err
	}
	w.ID, w.Principal, w.Target, w.Authority = "goalwatch-"+id, g.Domain.Owner, string(target), g.Execution.Digest
	return w, w.Validate()
}

// EventSourceAuthority is injected from the registered source adapters. Check
// must verify current source incarnation and every data-flow dependency.
type EventSourceAuthority interface {
	Check(context.Context, string, sessionevents.Source, []sessionevents.Dependency) error
}

type EventOccurrenceStore interface {
	MaterializeEvent(context.Context, Goal, sessionevents.Launch, time.Time) (Occurrence, error)
}

type EventExecution struct {
	Service  *Service
	Triggers *sessionevents.Triggers
	Sources  EventSourceAuthority
}

func (e *EventExecution) goal(ctx context.Context, sub sessionevents.Subscription) (Goal, error) {
	if e == nil || e.Service == nil || e.Service.Store == nil || e.Sources == nil {
		return Goal{}, ErrDenied
	}
	var target eventTarget
	if err := json.Unmarshal([]byte(sub.Target), &target); err != nil || target.Domain.Validate() != nil {
		return Goal{}, ErrDenied
	}
	g, err := e.Service.Store.Get(ctx, target.Domain, target.GoalID)
	if err != nil {
		return g, err
	}
	expected, err := EventSubscription(g)
	if err != nil || !reflect.DeepEqual(expected, sub) {
		return Goal{}, ErrDenied
	}
	return g, nil
}

func (e *EventExecution) CheckSubscription(ctx context.Context, sub sessionevents.Subscription) (result error) {
	defer func() {
		if errors.Is(result, ErrDenied) || errors.Is(result, ErrNotFound) {
			result = errors.Join(sessionevents.ErrDenied, result)
		}
	}()
	g, err := e.goal(ctx, sub)
	if err != nil {
		return err
	}
	c := g.Execution
	if e.Service.ExecutionAuth == nil || g.State != Active || c == nil || c.Decision == nil || !c.Decision.Approved || c.Decision.Owner != g.Domain.Owner || c.Decision.Digest != c.Digest || g.Revision != c.RequestRevision+1 || !e.Service.now().Before(c.Terms.ExpiresAt) {
		return ErrDenied
	}
	if err = e.Service.ExecutionAuth.VerifyDecision(ctx, g, *c.Decision); err != nil {
		return err
	}
	if err = e.Service.ExecutionAuth.Validate(ctx, g, c.Terms); err != nil {
		return err
	}
	if err = e.Service.ExecutionAuth.AuthorizeDispatch(ctx, g); err != nil {
		return err
	}
	return e.Sources.Check(ctx, g.Domain.Owner, sub.Source, nil)
}

func (e *EventExecution) CheckObservation(ctx context.Context, sub sessionevents.Subscription, observation sessionevents.Observation) (result error) {
	defer func() {
		if errors.Is(result, ErrDenied) || errors.Is(result, ErrNotFound) {
			result = errors.Join(sessionevents.ErrDenied, result)
		}
	}()
	g, err := e.goal(ctx, sub)
	if err != nil {
		return err
	}
	if observation.Source != sub.Source || observation.Validate() != nil {
		return ErrDenied
	}
	return e.Sources.Check(ctx, g.Domain.Owner, sub.Source, observation.Dependencies)
}

func (e *EventExecution) Activate(ctx context.Context, g Goal) error {
	if e == nil || e.Triggers == nil {
		return ErrDenied
	}
	sub, err := EventSubscription(g)
	if err != nil {
		return err
	}
	_, err = e.Triggers.Activate(ctx, sub)
	return err
}

func (e *EventExecution) Materialize(ctx context.Context, launch sessionevents.Launch) error {
	if e == nil || e.Triggers == nil || e.Service == nil {
		return ErrDenied
	}
	if err := e.Triggers.CheckLaunch(ctx, launch, e.Service.now()); err != nil {
		return err
	}
	g, err := e.goal(ctx, launch.Subscription)
	if err != nil {
		return err
	}
	store, ok := e.Service.Store.(EventOccurrenceStore)
	if !ok {
		return ErrDenied
	}
	_, err = store.MaterializeEvent(ctx, g, launch, e.Service.now())
	return err
}

// CheckOccurrence reads the retained accepted launch, including after outbox
// acknowledgment. A caller-selected window or observation can never substitute
// for the exact evidence that created the durable occurrence.
func (e *EventExecution) CheckOccurrence(ctx context.Context, g Goal, supplied Occurrence) (result error) {
	defer func() {
		if errors.Is(result, sessionevents.ErrDenied) || errors.Is(result, sessionevents.ErrNotFound) {
			result = errors.Join(ErrDenied, result)
		}
	}()
	if e == nil || e.Triggers == nil || e.Triggers.Store == nil || e.Service == nil {
		return ErrDenied
	}
	store, ok := e.Service.Store.(OccurrenceStore)
	if !ok {
		return ErrDenied
	}
	o, err := store.Occurrence(ctx, supplied.ID)
	if err != nil {
		return err
	}
	if o.Event == nil || !reflect.DeepEqual(o.Event, supplied.Event) || !o.DueAt.Equal(supplied.DueAt) || !o.ExpiresAt.Equal(supplied.ExpiresAt) || o.Domain != supplied.Domain || o.GoalID != supplied.GoalID || o.GoalRevision != supplied.GoalRevision || o.ConsentDigest != supplied.ConsentDigest {
		return ErrDenied
	}
	sub, err := EventSubscription(g)
	if err != nil || !reflect.DeepEqual(sub, o.Event.Subscription) {
		return ErrDenied
	}
	state, err := e.Triggers.Store.Subscription(ctx, sub.ID)
	if err != nil {
		return err
	}
	if state.Stopped || !reflect.DeepEqual(state.Subscription, sub) {
		return ErrDenied
	}
	if err = e.CheckSubscription(ctx, sub); err != nil {
		return err
	}
	return e.CheckObservation(ctx, sub, o.Event.Observation)
}
