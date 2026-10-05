// Package goals owns durable, user-scoped outcomes across agent sessions.
// An active goal describes desired work; it never authorizes execution.
package goals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid goal request")
	ErrConflict = errors.New("goal revision or request key conflict")
	ErrNotFound = errors.New("goal not found")
	ErrDenied   = errors.New("goal access denied")
)

type State string

const (
	Draft     State = "draft"
	Active    State = "active"
	Paused    State = "paused"
	Completed State = "completed"
	Cancelled State = "cancelled"
)

// Domain is resolved by a trusted server, never selected in tool arguments.
// ClassUID makes deletion and recreation of a class an ownership boundary.
type Domain struct {
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	Class     string `json:"class"`
	ClassUID  string `json:"classUID"`
}

func (d Domain) Validate() error {
	if d.Namespace == "" || d.Owner == "" || d.Class == "" || d.ClassUID == "" {
		return fmt.Errorf("%w: missing ownership", ErrInvalid)
	}
	return nil
}

func (d Domain) ID() string {
	b, _ := json.Marshal(d) // a struct of strings cannot fail to encode
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type PlanRef struct {
	SessionUID string `json:"sessionUID"`
	EntryID    string `json:"entryID"`
	Digest     string `json:"digest,omitempty"`
}

// Result is a reported outcome, not proof that external effects succeeded.
type Result struct {
	Summary  string   `json:"summary"`
	Evidence []string `json:"evidence"`
}

// Source preserves information-flow dependencies across sessions. It is resolved
// by the platform and cannot be supplied or removed by a goal request.
type Source struct {
	ResourceType string `json:"resourceType"`
	ResourceID   string `json:"resourceID"`
	Permission   string `json:"permission"`
}

type Goal struct {
	Sources       []Source          `json:"sources,omitempty"`
	ID            string            `json:"id"`
	Domain        Domain            `json:"domain"`
	Revision      int64             `json:"revision"`
	Title         string            `json:"title"`
	Outcome       string            `json:"outcome"`
	State         State             `json:"state"`
	DueAt         *time.Time        `json:"dueAt,omitempty"`
	Timezone      string            `json:"timezone,omitempty"`
	Plan          *PlanRef          `json:"plan,omitempty"`
	Result        *Result           `json:"result,omitempty"`
	Execution     *ExecutionConsent `json:"execution,omitempty"`
	OriginSession string            `json:"originSession"`
	CreatedAt     time.Time         `json:"createdAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
}

type CreateRequest struct {
	RequestID string     `json:"requestID"`
	Title     string     `json:"title"`
	Outcome   string     `json:"outcome"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
	Timezone  string     `json:"timezone,omitempty"`
}

// Change uses pointers to distinguish omitted text from an invalid empty edit.
// ClearDue removes a schedule intent; no schedule in this slice executes work.
type Change struct {
	RequestID string     `json:"requestID"`
	ID        string     `json:"id"`
	Revision  int64      `json:"revision"`
	Action    string     `json:"action"`
	Title     *string    `json:"title,omitempty"`
	Outcome   *string    `json:"outcome,omitempty"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
	Timezone  *string    `json:"timezone,omitempty"`
	ClearDue  bool       `json:"clearDue,omitempty"`
	Result    *Result    `json:"result,omitempty"`
}

type ListRequest struct {
	State State  `json:"state,omitempty"`
	After string `json:"after,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type Page struct {
	Goals              []Goal `json:"goals"`
	Next               string `json:"next,omitempty"`
	ExecutionAvailable bool   `json:"executionAvailable"`
}

// Event is committed in the same transaction as its goal revision.
type Event struct {
	ID         string          `json:"id"`
	ActorProof json.RawMessage `json:"actorProof,omitempty"`
	Goal       Goal            `json:"goal"`
	Action     string          `json:"action"`
	Session    string          `json:"session"`
	Proof      string          `json:"proof"`
	Occurrence *Occurrence     `json:"occurrence,omitempty"`
	OccurredAt *time.Time      `json:"occurredAt,omitempty"`
}

func validState(s State) bool {
	switch s {
	case Draft, Active, Paused, Completed, Cancelled:
		return true
	}
	return false
}

func validateGoal(g Goal) error {
	if err := g.Domain.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(g.Title) == "" || len(g.Title) > 256 || strings.TrimSpace(g.Outcome) == "" || len(g.Outcome) > 16384 || !validState(g.State) {
		return fmt.Errorf("%w: title, outcome, or state", ErrInvalid)
	}
	if g.Timezone != "" {
		if _, err := time.LoadLocation(g.Timezone); err != nil {
			return fmt.Errorf("%w: timezone", ErrInvalid)
		}
	}
	if g.Result != nil && (strings.TrimSpace(g.Result.Summary) == "" || len(g.Result.Summary) > 16384 || len(g.Result.Evidence) == 0 || len(g.Result.Evidence) > 32) {
		return fmt.Errorf("%w: completion requires summary and evidence", ErrInvalid)
	}
	if g.Result != nil {
		for _, ref := range g.Result.Evidence {
			if strings.TrimSpace(ref) == "" || len(ref) > 1024 {
				return fmt.Errorf("%w: completion evidence", ErrInvalid)
			}
		}
	}
	return nil
}

func Revise(g Goal, c Change, now time.Time) (Goal, error) {
	if g.Revision != c.Revision {
		return Goal{}, ErrConflict
	}
	if g.State == Completed || g.State == Cancelled {
		return Goal{}, fmt.Errorf("%w: terminal goal", ErrInvalid)
	}
	if c.Action != "revise" && (c.Title != nil || c.Outcome != nil || c.DueAt != nil || c.Timezone != nil || c.ClearDue) {
		return Goal{}, fmt.Errorf("%w: edits require revise", ErrInvalid)
	}
	if c.Action != "complete" && c.Result != nil {
		return Goal{}, fmt.Errorf("%w: result requires complete", ErrInvalid)
	}
	switch c.Action {
	case "revise":
		if c.Title == nil && c.Outcome == nil && c.DueAt == nil && c.Timezone == nil && !c.ClearDue {
			return Goal{}, fmt.Errorf("%w: revise requires an edited field", ErrInvalid)
		}
		if c.Title != nil {
			g.Title = *c.Title
		}
		if c.Outcome != nil {
			g.Outcome = *c.Outcome
		}
		if c.ClearDue && c.DueAt != nil {
			return Goal{}, fmt.Errorf("%w: conflicting due time", ErrInvalid)
		}
		if c.DueAt != nil {
			t := c.DueAt.UTC()
			g.DueAt = &t
		}
		if c.ClearDue {
			g.DueAt = nil
		}
		if c.Timezone != nil {
			g.Timezone = *c.Timezone
		}
	case "activate":
		if g.State != Draft {
			return Goal{}, fmt.Errorf("%w: activate requires draft", ErrInvalid)
		}
		g.State = Active
	case "pause":
		if g.State != Active {
			return Goal{}, fmt.Errorf("%w: pause requires active", ErrInvalid)
		}
		g.State = Paused
	case "resume":
		if g.State != Paused {
			return Goal{}, fmt.Errorf("%w: resume requires paused", ErrInvalid)
		}
		g.State = Active
	case "cancel":
		g.State = Cancelled
	case "complete":
		if c.Result == nil {
			return Goal{}, fmt.Errorf("%w: completion evidence required", ErrInvalid)
		}
		g.State = Completed
		g.Result = c.Result
	default:
		return Goal{}, fmt.Errorf("%w: unknown action", ErrInvalid)
	}
	g.Revision++
	// Any management mutation conservatively requires fresh execution consent.
	// In particular, resume must never revive authority withdrawn by pause.
	g.Execution = nil
	g.UpdatedAt = now.UTC()
	return g, validateGoal(g)
}
