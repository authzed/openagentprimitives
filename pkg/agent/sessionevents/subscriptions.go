package sessionevents

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/authzed/openagentprimitives/pkg/agent/sessionschedule"
)

// Predicate is a literal match, never a program or instruction. Equals compares
// only top-level string fields; missing, nested and non-string values do not match.
type Predicate struct {
	Kind    string            `json:"kind"`
	Subject string            `json:"subject"`
	Equals  map[string]string `json:"equals,omitempty"`
}

func validText(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 1024 && utf8.ValidString(value)
}
func (p Predicate) Validate() error {
	if !validText(p.Kind) || !validText(p.Subject) || len(p.Equals) > 16 {
		return ErrInvalid
	}
	for k, v := range p.Equals {
		if !validText(k) || len(v) > 1024 || !utf8.ValidString(v) {
			return ErrInvalid
		}
	}
	return nil
}
func (p Predicate) Matches(o Observation) bool {
	if p.Kind != o.Kind || p.Subject != o.Subject {
		return false
	}
	if len(p.Equals) == 0 {
		return true
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(o.Data, &fields) != nil {
		return false
	}
	for key, value := range p.Equals {
		var actual *string
		raw, ok := fields[key]
		if !ok || json.Unmarshal(raw, &actual) != nil || actual == nil || *actual != value {
			return false
		}
	}
	return true
}

// Subscription is the immutable reviewed watch. Authority is an opaque digest
// resolved by the consumer's live authority adapter. Target identifies that
// consumer's durable work; neither field is itself a permission grant.
type Subscription struct {
	ID               string                       `json:"id"`
	Principal        string                       `json:"principal"`
	Target           string                       `json:"target"`
	Authority        string                       `json:"authority"`
	Source           Source                       `json:"source"`
	Predicate        Predicate                    `json:"predicate"`
	StartsAt         time.Time                    `json:"startsAt"`
	EndsAt           time.Time                    `json:"endsAt"`
	MaxRuns          int                          `json:"maxRuns"`
	RunWindowSeconds int64                        `json:"runWindowSeconds"`
	Timezone         string                       `json:"timezone"`
	QuietHours       []sessionschedule.QuietHours `json:"quietHours,omitempty"`
	// SkipPending retains but skips additional matching events while a launch
	// is pending. It never replaces the first event with newer evidence.
	Burst string `json:"burst"`
}

func (s Subscription) Validate() error {
	if !validText(s.ID) || !validText(s.Principal) || !validText(s.Target) || !validText(s.Authority) ||
		s.Source.Validate() != nil || s.Predicate.Validate() != nil || s.StartsAt.IsZero() || !s.EndsAt.After(s.StartsAt) ||
		s.EndsAt.Sub(s.StartsAt) > sessionschedule.MaxHorizon || s.MaxRuns < 1 || s.MaxRuns > sessionschedule.MaxRuns ||
		s.RunWindowSeconds < 1 || s.RunWindowSeconds > 86400 || s.Burst != "skip_pending" {
		return ErrInvalid
	}
	_, _, err := sessionschedule.EventWindow(s.Timezone, s.QuietHours, s.StartsAt, s.EndsAt)
	if err != nil {
		return ErrInvalid
	}
	return nil
}
func (s Subscription) Digest() (string, error) { return jsonDigest(s) }

type SubscriptionState struct {
	Subscription Subscription `json:"subscription"`
	Used         int          `json:"used"`
	Stopped      bool         `json:"stopped"`
	StopReason   string       `json:"stopReason,omitempty"`
}

// Admission is immutable, including skipped matches. LaunchID is deterministic;
// retrying admission or losing a launch acknowledgment consumes no further run.
type Admission struct {
	SubscriptionID string                 `json:"subscriptionID"`
	ObservationID  string                 `json:"observationID"`
	LaunchID       string                 `json:"launchID,omitempty"`
	Disposition    string                 `json:"disposition"`
	AcceptedAt     time.Time              `json:"acceptedAt"`
	Window         sessionschedule.Window `json:"window"`
}

func LaunchID(subscriptionID, observationID string) string {
	return "evlaunch-" + mustDigest(struct{ SubscriptionID, ObservationID string }{subscriptionID, observationID})
}

type Launch struct {
	Admission    Admission    `json:"admission"`
	Subscription Subscription `json:"subscription"`
	Observation  Observation  `json:"observation"`
}

// TriggerStore is trusted internal persistence, not an authorization API. Admit
// loads immutable stored evidence, then commits admission, allowance and launch
// intent in one transaction. Pending is an outbox; consumers durably create work
// idempotently under LaunchID before acknowledging it.
type TriggerStore interface {
	CreateSubscription(context.Context, Subscription) (SubscriptionState, error)
	Subscription(context.Context, string) (SubscriptionState, error)
	Admit(context.Context, string, Source, string, time.Time) (Admission, error)
	Pending(context.Context, time.Time, int) ([]Launch, error)
	Launch(context.Context, string) (Launch, error)
	Acknowledge(context.Context, string) error
	StopSubscription(context.Context, string, string) error
}

// TriggerAuthority rechecks the reviewed consumer authority, live principal,
// exact source incarnation and all observation flow dependencies. It must also
// be checked by the consumer at activation and every permissioned effect.
type TriggerAuthority interface {
	CheckSubscription(context.Context, Subscription) error
	CheckObservation(context.Context, Subscription, Observation) error
}
type Triggers struct {
	Store        TriggerStore
	Observations Store
	Authority    TriggerAuthority
}

func (t *Triggers) Activate(ctx context.Context, s Subscription) (SubscriptionState, error) {
	if t == nil || t.Store == nil || t.Authority == nil {
		return SubscriptionState{}, ErrDenied
	}
	if err := s.Validate(); err != nil {
		return SubscriptionState{}, err
	}
	s.StartsAt = s.StartsAt.UTC()
	s.EndsAt = s.EndsAt.UTC()
	if err := t.Authority.CheckSubscription(ctx, s); err != nil {
		return SubscriptionState{}, err
	}
	return t.Store.CreateSubscription(ctx, s)
}
func (t *Triggers) Accept(ctx context.Context, id string, source Source, eventID string, now time.Time) (Admission, error) {
	if t == nil || t.Store == nil || t.Observations == nil || t.Authority == nil {
		return Admission{}, ErrDenied
	}
	state, err := t.Store.Subscription(ctx, id)
	if err != nil {
		return Admission{}, err
	}
	if state.Subscription.Source != source {
		return Admission{}, ErrDenied
	}
	if err = t.Authority.CheckSubscription(ctx, state.Subscription); err != nil {
		return Admission{}, err
	}
	observation, err := t.Observations.Get(ctx, source, eventID)
	if err != nil {
		return Admission{}, err
	}
	if err = t.Authority.CheckObservation(ctx, state.Subscription, observation); err != nil {
		return Admission{}, err
	}
	return t.Store.Admit(ctx, id, source, eventID, now)
}

// CheckLaunch is required again after polling an outbox. Persistence is not
// authority: a revoked or replaced source cannot use previously accepted data.
func (t *Triggers) CheckLaunch(ctx context.Context, launch Launch, now time.Time) error {
	if t == nil || t.Store == nil || t.Authority == nil {
		return ErrDenied
	}
	stored, err := t.Store.Launch(ctx, launch.Admission.LaunchID)
	if err != nil {
		return err
	}
	original, err := jsonDigest(stored)
	if err != nil {
		return err
	}
	received, err := jsonDigest(launch)
	if err != nil {
		return err
	}
	if original != received || now.Before(stored.Admission.Window.DueAt) || !now.Before(stored.Admission.Window.ExpiresAt) {
		return ErrDenied
	}
	state, err := t.Store.Subscription(ctx, launch.Subscription.ID)
	if err != nil {
		return err
	}
	if state.Stopped {
		return ErrDenied
	}
	expected, err := state.Subscription.Digest()
	if err != nil {
		return err
	}
	supplied, err := launch.Subscription.Digest()
	if err != nil {
		return err
	}
	if expected != supplied || launch.Observation.Source != state.Subscription.Source {
		return ErrDenied
	}
	if err = t.Authority.CheckSubscription(ctx, state.Subscription); err != nil {
		return err
	}
	return t.Authority.CheckObservation(ctx, state.Subscription, launch.Observation)
}
