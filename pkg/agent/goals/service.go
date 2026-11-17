package goals

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Actor is produced by the authenticated transport after checking current
// permissions and a platform-authored inbound record. It is not a wire input.
type Actor struct {
	Domain         Domain
	Session, Proof string
	SessionUID     string
	// Attestation is the verified channelsd envelope, retained in each audit
	// event so session cleanup cannot erase its authorship evidence.
	Attestation string
	// User and TurnRef are set only for a group-owned Domain, where the
	// person acting is not the owner: User is the attested member's canonical
	// id, and TurnRef the inbound whose attestation named them, so the actor
	// can be re-derived for the same turn. Both stay empty for private scope.
	User, TurnRef string
}

// GroupAccess names a check a group-owned domain needs beyond read and
// manage, which go through Authorizer.Authorize as for private goals.
type GroupAccess string

const (
	// GroupMember: the actor currently resolves as a member of the owning
	// group. Creating needs only this; the creator becomes the first manager.
	GroupMember GroupAccess = "member"
	// GroupPause: any member may pause (agent_goal_domain#pause).
	GroupPause GroupAccess = "pause"
	// GroupRelinquish: manage, for a change that only takes authority away
	// (cancel, remove_manager). It skips the source and audience checks a
	// write otherwise runs: those stop a session from carrying data INTO a
	// goal, and a change that stops a goal carries nothing.
	GroupRelinquish GroupAccess = "relinquish"
)

// RelinquishesAuthority reports whether a change only takes authority away
// from a goal. Such a change never merges the session's sources into the goal,
// so an emergency stop cannot be blocked by what the session has read.
func RelinquishesAuthority(action string) bool {
	return action == "pause" || action == "cancel" || action == "remove_manager"
}

// GroupAuthorizer is the half of an Authorizer that serves group-owned
// domains. Kept separate so private-only authorizers need not implement it;
// a Service whose Authorizer lacks it refuses every group request.
type GroupAuthorizer interface {
	AuthorizeGroup(context.Context, Actor, GroupAccess) error
	// CheckManagers validates a manager change already applied to g by
	// Revise: an added manager must be in the group, and the change must leave
	// at least one manager who still is.
	CheckManagers(context.Context, Actor, Goal, Change) error
	// AuthorizeGoalManager requires the actor to be listed in g.Managers, or
	// to administer the goal's class. It backs the domain's manage check with
	// the goal's own record, so a manager tuple that outlived a removal never
	// grants management on its own.
	AuthorizeGoalManager(context.Context, Actor, Goal) error
}

type Authorizer interface {
	Authorize(context.Context, Actor, bool) error
	Sources(context.Context, Actor) ([]Source, error)
	ReadGoal(context.Context, Actor, Goal) error
}

type Service struct {
	Store         Store
	Auth          Authorizer
	ExecutionAuth ExecutionAuthority
	Events        *EventExecution
	Now           func() time.Time
	// GroupLinks lists the member subject-set types ("group#member") a group
	// owner may use. Injected so this package stays free of channel imports;
	// nil means no group domain is valid.
	GroupLinks func() []string
}

// validateDomain extends Domain.Validate with the group-owner rule: a group
// owner must be a well-formed subject set whose "<type>#<relation>" is a
// registered member link.
func (s *Service) validateDomain(d Domain) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if !d.GroupOwned() {
		return nil
	}
	typ, _, rel, ok := d.OwnerSubjectSet()
	if !ok {
		return fmt.Errorf("%w: malformed group owner", ErrInvalid)
	}
	if s.GroupLinks != nil && slices.Contains(s.GroupLinks(), typ+"#"+rel) {
		return nil
	}
	return fmt.Errorf("%w: group owner %s#%s is not a registered member link", ErrInvalid, typ, rel)
}

func (s *Service) authorize(ctx context.Context, a Actor, write bool) error {
	if s.Auth == nil || a.Session == "" || a.Proof == "" {
		return ErrDenied
	}
	if err := s.validateDomain(a.Domain); err != nil {
		return ErrDenied
	}
	return s.Auth.Authorize(ctx, a, write)
}

// groupAuthorizer runs authorize's preconditions for a group-owned domain and
// returns the GroupAuthorizer to ask.
func (s *Service) groupAuthorizer(a Actor) (GroupAuthorizer, error) {
	if s.Auth == nil || a.Session == "" || a.Proof == "" || a.User == "" || !a.Domain.GroupOwned() {
		return nil, ErrDenied
	}
	if err := s.validateDomain(a.Domain); err != nil {
		return nil, ErrDenied
	}
	g, ok := s.Auth.(GroupAuthorizer)
	if !ok {
		return nil, fmt.Errorf("%w: group goals are not served here", ErrDenied)
	}
	return g, nil
}

func (s *Service) authorizeGroup(ctx context.Context, a Actor, access GroupAccess) error {
	g, err := s.groupAuthorizer(a)
	if err != nil {
		return err
	}
	return g.AuthorizeGroup(ctx, a, access)
}

// authorizeCreate: a private create needs manage on the actor's own domain;
// a group create needs only membership, since nobody manages the goal yet.
func (s *Service) authorizeCreate(ctx context.Context, a Actor) error {
	if a.Domain.GroupOwned() {
		return s.authorizeGroup(ctx, a, GroupMember)
	}
	return s.authorize(ctx, a, true)
}

// authorizeChange: pausing a group goal is any member's emergency brake;
// every other change, and every private change, needs manage.
func (s *Service) authorizeChange(ctx context.Context, a Actor, c Change) error {
	if a.Domain.GroupOwned() {
		switch {
		case c.Action == "pause":
			return s.authorizeGroup(ctx, a, GroupPause)
		case RelinquishesAuthority(c.Action):
			return s.authorizeGroup(ctx, a, GroupRelinquish)
		}
	}
	return s.authorize(ctx, a, true)
}

// requireGroupManager checks a manage-level group write against the goal's
// own manager list (or class administration). Pause is any member's.
func (s *Service) requireGroupManager(ctx context.Context, a Actor, g Goal, action string) error {
	if !a.Domain.GroupOwned() || action == "pause" {
		return nil
	}
	ga, err := s.groupAuthorizer(a)
	if err != nil {
		return err
	}
	return ga.AuthorizeGoalManager(ctx, a, g)
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func requestHash(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("encode goal request: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func newID(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate goal ID: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

func validRequestID(id string) bool { return len(id) > 0 && len(id) <= 128 }

func (s *Service) Create(ctx context.Context, a Actor, r CreateRequest) (Goal, error) {
	if err := s.authorizeCreate(ctx, a); err != nil {
		return Goal{}, err
	}
	if !validRequestID(r.RequestID) {
		return Goal{}, fmt.Errorf("%w: requestID required", ErrInvalid)
	}
	h, err := requestHash(r)
	if err != nil {
		return Goal{}, err
	}
	if g, ok, err := s.Store.Receipt(ctx, a.Domain, r.RequestID, h); err != nil || ok {
		if err == nil {
			err = s.Auth.ReadGoal(ctx, a, g)
		}
		return g, err
	}
	id, err := newID("goal-")
	if err != nil {
		return Goal{}, err
	}
	now := s.now()
	g := Goal{ID: id, Domain: a.Domain, Revision: 1, Title: r.Title, Outcome: r.Outcome, State: Draft, DueAt: r.DueAt, Timezone: r.Timezone, OriginSession: a.Session, CreatedAt: now, UpdatedAt: now}
	if g.DueAt != nil {
		t := g.DueAt.UTC()
		g.DueAt = &t
	}
	if a.Domain.GroupOwned() {
		g.Managers = []string{a.User}
	}
	g.Sources, err = s.Auth.Sources(ctx, a)
	if err != nil {
		return Goal{}, err
	}
	if err := validateGoal(g); err != nil {
		return Goal{}, err
	}
	return s.commit(ctx, a, g, 0, r.RequestID, h, "create")
}

func (s *Service) Get(ctx context.Context, a Actor, id string) (Goal, error) {
	if err := s.authorize(ctx, a, false); err != nil {
		return Goal{}, err
	}
	g, err := s.Store.Get(ctx, a.Domain, id)
	if err != nil {
		return Goal{}, err
	}
	if err := s.Auth.ReadGoal(ctx, a, g); err != nil {
		return Goal{}, err
	}
	return g, nil
}

func (s *Service) List(ctx context.Context, a Actor, r ListRequest) (Page, error) {
	if err := s.authorize(ctx, a, false); err != nil {
		return Page{}, err
	}
	if r.Limit == 0 {
		r.Limit = 50
	}
	if r.Limit < 1 || r.Limit > 100 || r.State != "" && !validState(r.State) {
		return Page{}, ErrInvalid
	}
	p, err := s.Store.List(ctx, a.Domain, r)
	if err != nil {
		return Page{}, err
	}
	visible := make([]Goal, 0, len(p.Goals))
	for _, g := range p.Goals {
		if err := s.Auth.ReadGoal(ctx, a, g); err != nil {
			if errors.Is(err, ErrDenied) {
				continue
			}
			return Page{}, err
		}
		visible = append(visible, g)
	}
	p.Goals = visible
	return p, nil
}

func (s *Service) Update(ctx context.Context, a Actor, r Change) (Goal, error) {
	if err := s.authorizeChange(ctx, a, r); err != nil {
		return Goal{}, err
	}
	if !validRequestID(r.RequestID) || r.Revision < 1 {
		return Goal{}, ErrInvalid
	}
	h, err := requestHash(r)
	if err != nil {
		return Goal{}, err
	}
	if g, ok, err := s.Store.Receipt(ctx, a.Domain, r.RequestID, h); err != nil || ok {
		if err == nil {
			err = s.Auth.ReadGoal(ctx, a, g)
		}
		return g, err
	}
	g, err := s.Store.Get(ctx, a.Domain, r.ID)
	if err != nil {
		return Goal{}, err
	}
	if err := s.Auth.ReadGoal(ctx, a, g); err != nil {
		return Goal{}, err
	}
	if err := s.requireGroupManager(ctx, a, g, r.Action); err != nil {
		return Goal{}, err
	}
	// A group change that only takes authority away carries no session data
	// into the goal, so it neither merges nor checks the session's sources.
	if !a.Domain.GroupOwned() || !RelinquishesAuthority(r.Action) {
		sources, err := s.Auth.Sources(ctx, a)
		if err != nil {
			return Goal{}, err
		}
		g.Sources = mergeSources(g.Sources, sources)
	}
	next, err := Revise(g, r, s.now())
	if err != nil {
		return Goal{}, err
	}
	if IsManagerAction(r.Action) {
		ga, err := s.groupAuthorizer(a)
		if err != nil {
			return Goal{}, err
		}
		if err := ga.CheckManagers(ctx, a, next, r); err != nil {
			return Goal{}, err
		}
	}
	return s.commit(ctx, a, next, r.Revision, r.RequestID, h, r.Action)
}

func mergeSources(existing, added []Source) []Source {
	sources := append(existing, added...)
	slices.SortFunc(sources, func(a, b Source) int {
		if n := cmp.Compare(a.ResourceType, b.ResourceType); n != 0 {
			return n
		}
		if n := cmp.Compare(a.ResourceID, b.ResourceID); n != 0 {
			return n
		}
		return cmp.Compare(a.Permission, b.Permission)
	})
	return slices.Compact(sources)
}

func (s *Service) commit(ctx context.Context, a Actor, g Goal, expected int64, key, hash, action string) (Goal, error) {
	id, err := newID("goalev-")
	if err != nil {
		return Goal{}, err
	}
	accepted, err := s.Store.Commit(ctx, Mutation{Goal: g, Expected: expected, RequestID: key, Hash: hash, Event: Event{ID: id, Goal: g, Action: action, Session: a.Session, Proof: a.Proof, ActorProof: json.RawMessage(a.Attestation)}})
	if err != nil {
		return Goal{}, err
	}
	// A concurrent identical request can win after the receipt lookup, carrying
	// dependencies from another session. Check the actual accepted snapshot
	// before returning it, just as on the ordinary receipt replay path.
	if err := s.Auth.ReadGoal(ctx, a, accepted); err != nil {
		return Goal{}, err
	}
	return accepted, nil
}
