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
	// Attestation is the verified channelsd envelope, retained in each audit
	// event so session cleanup cannot erase its authorship evidence.
	Attestation string
}
type Authorizer interface {
	Authorize(context.Context, Actor, bool) error
	Sources(context.Context, Actor) ([]Source, error)
	ReadGoal(context.Context, Actor, Goal) error
}
type Service struct {
	Store Store
	Auth  Authorizer
	Now   func() time.Time
}

func (s *Service) authorize(ctx context.Context, a Actor, write bool) error {
	if s.Auth == nil || a.Session == "" || a.Proof == "" {
		return ErrDenied
	}
	if err := a.Domain.Validate(); err != nil {
		return ErrDenied
	}
	return s.Auth.Authorize(ctx, a, write)
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
	if err := s.authorize(ctx, a, true); err != nil {
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
	if err := s.authorize(ctx, a, true); err != nil {
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
	sources, err := s.Auth.Sources(ctx, a)
	if err != nil {
		return Goal{}, err
	}
	g.Sources = append(g.Sources, sources...)
	slices.SortFunc(g.Sources, func(a, b Source) int {
		if n := cmp.Compare(a.ResourceType, b.ResourceType); n != 0 {
			return n
		}
		if n := cmp.Compare(a.ResourceID, b.ResourceID); n != 0 {
			return n
		}
		return cmp.Compare(a.Permission, b.Permission)
	})
	g.Sources = slices.Compact(g.Sources)
	next, err := Revise(g, r, s.now())
	if err != nil {
		return Goal{}, err
	}
	return s.commit(ctx, a, next, r.Revision, r.RequestID, h, r.Action)
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
