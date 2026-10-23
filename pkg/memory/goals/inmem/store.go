// Package inmem implements the goal storage contract for explicit ephemeral use.
package inmem

import (
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/goals"
)

type receipt struct {
	hash string
	goal json.RawMessage
}

type event struct {
	data      json.RawMessage
	envelope  json.RawMessage
	published bool
}

type Store struct {
	mu       sync.Mutex
	rows     map[string]json.RawMessage
	receipts map[string]receipt
	events   map[string]event
	order    []string
}

func New() *Store {
	return &Store{rows: map[string]json.RawMessage{}, receipts: map[string]receipt{}, events: map[string]event{}}
}

var _ goals.Store = (*Store)(nil)

func key(d goals.Domain, id string) string { return d.ID() + "/" + id }
func decode(b json.RawMessage) (goals.Goal, error) {
	var g goals.Goal
	err := json.Unmarshal(b, &g)
	return g, err
}

func (s *Store) Get(_ context.Context, d goals.Domain, id string) (goals.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.rows[key(d, id)]
	if !ok {
		return goals.Goal{}, goals.ErrNotFound
	}
	return decode(b)
}

func (s *Store) List(_ context.Context, d goals.Domain, r goals.ListRequest) (goals.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := goals.Page{Goals: []goals.Goal{}}
	for _, b := range s.rows {
		g, err := decode(b)
		if err != nil {
			return goals.Page{}, err
		}
		if g.Domain == d && g.ID > r.After && (r.State == "" || g.State == r.State) {
			p.Goals = append(p.Goals, g)
		}
	}
	sort.Slice(p.Goals, func(i, j int) bool { return p.Goals[i].ID < p.Goals[j].ID })
	if len(p.Goals) > r.Limit {
		p.Goals = p.Goals[:r.Limit]
		p.Next = p.Goals[len(p.Goals)-1].ID
	}
	return p, nil
}

func (s *Store) receipt(d goals.Domain, id, hash string) (goals.Goal, bool, error) {
	r, ok := s.receipts[key(d, id)]
	if !ok {
		return goals.Goal{}, false, nil
	}
	if r.hash != hash {
		return goals.Goal{}, false, goals.ErrConflict
	}
	g, err := decode(r.goal)
	return g, true, err
}

func (s *Store) Receipt(_ context.Context, d goals.Domain, id, hash string) (goals.Goal, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.receipt(d, id, hash)
}

func (s *Store) Commit(_ context.Context, m goals.Mutation) (goals.Goal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g, ok, err := s.receipt(m.Goal.Domain, m.RequestID, m.Hash); err != nil || ok {
		return g, err
	}
	b, exists := s.rows[key(m.Goal.Domain, m.Goal.ID)]
	if m.Expected == 0 && exists || m.Expected != 0 && !exists {
		return goals.Goal{}, goals.ErrConflict
	}
	if exists {
		old, err := decode(b)
		if err != nil {
			return goals.Goal{}, err
		}
		if old.Revision != m.Expected {
			return goals.Goal{}, goals.ErrConflict
		}
	}
	gb, err := json.Marshal(m.Goal)
	if err != nil {
		return goals.Goal{}, err
	}
	eb, err := json.Marshal(m.Event)
	if err != nil {
		return goals.Goal{}, err
	}
	s.rows[key(m.Goal.Domain, m.Goal.ID)] = gb
	s.receipts[key(m.Goal.Domain, m.RequestID)] = receipt{m.Hash, gb}
	s.events[m.Event.ID] = event{data: eb}
	s.order = append(s.order, m.Event.ID)
	return decode(gb)
}

func (s *Store) Pending(_ context.Context, limit int) ([]goals.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []goals.Event{}
	ordered := append([]string(nil), s.order...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(s.events[ordered[i]].envelope) > 0 && len(s.events[ordered[j]].envelope) == 0
	})
	for _, id := range ordered {
		e := s.events[id]
		if !e.published {
			var v goals.Event
			if err := json.Unmarshal(e.data, &v); err != nil {
				return nil, err
			}
			out = append(out, v)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func (s *Store) SaveEnvelope(_ context.Context, id string, b json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return goals.ErrNotFound
	}
	if len(e.envelope) > 0 && string(e.envelope) != string(b) {
		return goals.ErrConflict
	}
	e.envelope = append(json.RawMessage(nil), b...)
	s.events[id] = e
	return nil
}

func (s *Store) Envelope(_ context.Context, id string) (json.RawMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return nil, goals.ErrNotFound
	}
	return append(json.RawMessage(nil), e.envelope...), nil
}

func (s *Store) Published(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.events[id]
	if !ok {
		return goals.ErrNotFound
	}
	if len(e.envelope) == 0 {
		return goals.ErrConflict
	}
	e.published = true
	s.events[id] = e
	return nil
}
