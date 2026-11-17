// Package goals owns durable, user-scoped outcomes across agent sessions.
// An active goal describes desired work; it never authorizes execution.
package goals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid goal request")
	ErrConflict = errors.New("goal revision or request key conflict")
	ErrNotFound = errors.New("goal not found")
	ErrDenied   = errors.New("goal access denied")
)

// Causes of a group run's refusal that can never clear without a fresh
// approval. They travel alongside ErrDenied (an error wraps both), so a
// dispatcher records the specific RunReason while every ErrDenied check still
// holds. A refusal that wraps none of them is either transient or generic.
var (
	// ErrGroupChannelGone: the pinned Channel is deleted or recreated.
	ErrGroupChannelGone = errors.New("the group's channel is gone")
	// ErrGroupNotHostable: the Channel's kind, membership, member ref or the
	// class's owner ceiling no longer allow a group run.
	ErrGroupNotHostable = errors.New("the group's conversation can no longer host the run")
	// ErrMembershipNotCurrent: the group's membership is not kept current right
	// now. Unlike the others it is transient (a directory sync clears it), so a
	// dispatcher retries within the occurrence's window and records nothing.
	ErrMembershipNotCurrent = errors.New("group membership is not kept current")
	// ErrLookupFailed: an object the terms pin could not be read (not that it
	// is absent). Transient, retried like ErrMembershipNotCurrent.
	ErrLookupFailed = errors.New("lookup failed")
	// ErrIdentityChanged: the class's AgentIdentity is deleted or recreated.
	ErrIdentityChanged = errors.New("the agent's identity changed since review")
)

// LastManagerRefusal is the refusal text for a manager change that would leave a
// group goal with no manager in the group. Platform admins can still step in,
// but they are not the group's managers, so they do not count.
const LastManagerRefusal = "a group goal needs a manager in the group"

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

// GroupOwned reports whether Owner is a member subject-set such as
// "group:g1#member" rather than a user id. Canonical user ids never contain
// '#', so the marker is unambiguous.
func (d Domain) GroupOwned() bool {
	return strings.Contains(d.Owner, "#")
}

// ManageRef names what a group goal's deciders hold: manage on
// agent_goal:<id>. Managers are per goal, so the consent card, the decision
// handler and DecideExecution all check this one ref, never the domain.
func (g Goal) ManageRef() (resourceType, resourceID, permission string) {
	return "agent_goal", g.ID, "manage"
}

// AcceptsDecider reports whether a recorded decision's Owner is one this
// domain's goals accept: the owner for a private goal; for a group goal, a
// non-empty individual — the approving manager, whose standing was checked
// live when they decided and is re-checked per run, not here.
func (d Domain) AcceptsDecider(owner string) bool {
	if !d.GroupOwned() {
		return owner == d.Owner
	}
	return owner != "" && !strings.Contains(owner, "#")
}

// OwnerSubjectSet splits a group Owner "<type>:<id>#<relation>". ok is false
// for a user owner or any malformed shape.
func (d Domain) OwnerSubjectSet() (typ, id, rel string, ok bool) {
	head, rel, found := strings.Cut(d.Owner, "#")
	if !found || strings.Contains(rel, "#") {
		return "", "", "", false
	}
	typ, id, found = strings.Cut(head, ":")
	if !found || typ == "" || id == "" || rel == "" {
		return "", "", "", false
	}
	return typ, id, rel, true
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
	Discovery *DiscoveryReference `json:"discovery,omitempty"`
	Sources   []Source            `json:"sources,omitempty"`
	ID        string              `json:"id"`
	Domain    Domain              `json:"domain"`
	Revision  int64               `json:"revision"`
	Title     string              `json:"title"`
	Outcome   string              `json:"outcome"`
	State     State               `json:"state"`
	// DueAt records descriptive intent; Execution.Terms pins executable authority.
	DueAt     *time.Time        `json:"dueAt,omitempty"`
	Timezone  string            `json:"timezone,omitempty"`
	Plan      *PlanRef          `json:"plan,omitempty"`
	Result    *Result           `json:"result,omitempty"`
	Execution *ExecutionConsent `json:"execution,omitempty"`
	// PendingRenewal is a group goal's renewal of Execution awaiting a
	// manager's decision. Execution stays in force until its own ExpiresAt
	// while it is pending. Neither recording nor deciding one changes the
	// revision; approving it replaces Execution. A declined renewal stays here, decided,
	// so the same approval is not asked to be renewed again.
	PendingRenewal *ExecutionConsent `json:"pendingRenewal,omitempty"`
	// PriorExecution is the approval a renewal replaced. It keeps governing
	// the runs it scheduled (occurrences bound to its digest) until its own
	// ExpiresAt, so approving a renewal supersedes nothing (ForConsent).
	PriorExecution *ExecutionConsent `json:"priorExecution,omitempty"`
	// LastApprovedTerms are the terms of a group goal's most recent approval.
	// Unlike Execution they survive pause, resume and edits, so a resume can
	// offer them for re-approval in one click.
	LastApprovedTerms *ExecutionTerms `json:"lastApprovedTerms,omitempty"`
	// BlockedReason is derived when a goal is read (Service.Get and List) and
	// never stored: commit clears it.
	BlockedReason string `json:"blockedReason,omitempty"`
	// Managers are the canonical user ids that may manage a group-owned
	// goal, as long as they stay in the group (the schema intersects manager
	// with member_group). Empty for private goals. The agent_goal_domain
	// manager tuples are written from this list, so it is the source of truth
	// a failed tuple write is repaired from.
	Managers      []string  `json:"managers,omitempty"`
	OriginSession string    `json:"originSession"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type CreateRequest struct {
	RequestID string     `json:"requestID"`
	Title     string     `json:"title"`
	Outcome   string     `json:"outcome"`
	DueAt     *time.Time `json:"dueAt,omitempty"`
	Timezone  string     `json:"timezone,omitempty"`
}

// Change uses pointers to distinguish omitted text from an invalid empty edit.
// ClearDue removes descriptive due intent. Executable schedules are separately
// reviewed and pinned in Execution.Terms.
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
	// Manager names the canonical user id an add_manager or remove_manager
	// action adds or removes. Group goals only.
	Manager *string `json:"manager,omitempty"`
}

// IsManagerAction reports whether action changes who manages a group goal.
func IsManagerAction(action string) bool {
	return action == "add_manager" || action == "remove_manager"
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
	// Actor names a system author, for an event no session's person caused
	// (a renewal request). Empty when Session and Proof name the actor.
	Actor      string      `json:"actor,omitempty"`
	Session    string      `json:"session"`
	Proof      string      `json:"proof"`
	Occurrence *Occurrence `json:"occurrence,omitempty"`
	OccurredAt *time.Time  `json:"occurredAt,omitempty"`
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
	if !IsManagerAction(c.Action) && c.Manager != nil {
		return Goal{}, fmt.Errorf("%w: manager requires add_manager or remove_manager", ErrInvalid)
	}
	if IsManagerAction(c.Action) {
		return reviseManagers(g, c, now)
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
	// A pending renewal and a renewed approval go with it; LastApprovedTerms
	// stays for resume.
	g.Execution = nil
	g.PendingRenewal = nil
	g.PriorExecution = nil
	g.UpdatedAt = now.UTC()
	return g, validateGoal(g)
}

// reviseManagers applies add_manager / remove_manager. A manager change says
// who may manage the goal, not what its runs may do, so unlike every other
// mutation it keeps the goal's execution consent — and its revision, which
// that consent and every scheduled or running occurrence are bound to; it is
// committed in place (InPlaceStore). Removing the last listed
// manager is refused here; the server additionally refuses a change that would
// leave no manager who is still in the group (CheckManagers), because the
// list alone cannot see membership.
func reviseManagers(g Goal, c Change, now time.Time) (Goal, error) {
	if !g.Domain.GroupOwned() {
		return Goal{}, fmt.Errorf("%w: managers apply only to group goals", ErrInvalid)
	}
	if c.Manager == nil || strings.TrimSpace(*c.Manager) == "" {
		return Goal{}, fmt.Errorf("%w: %s names no manager", ErrInvalid, c.Action)
	}
	who := *c.Manager
	at := slices.Index(g.Managers, who)
	managers := slices.Clone(g.Managers)
	switch c.Action {
	case "add_manager":
		if at >= 0 {
			return Goal{}, fmt.Errorf("%w: %s is already a manager", ErrInvalid, who)
		}
		managers = append(managers, who)
	case "remove_manager":
		if at < 0 {
			return Goal{}, fmt.Errorf("%w: %s is not a manager", ErrInvalid, who)
		}
		if len(managers) == 1 {
			return Goal{}, fmt.Errorf("%w: %s", ErrInvalid, LastManagerRefusal)
		}
		managers = slices.Delete(managers, at, at+1)
	}
	g.Managers = managers
	g.UpdatedAt = now.UTC()
	return g, validateGoal(g)
}
