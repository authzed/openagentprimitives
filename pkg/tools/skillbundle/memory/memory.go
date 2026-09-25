// Package memory is the in-memory skillbundle.Store backend. Lives in the
// operator process; suitable for the controller's own materialization use and
// for tests. Not durable across pod restarts and not visible to other pods —
// use the postgres backend when either matters.
package memory

import (
	"context"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/tools/skillbundle"
)

// Store is an in-memory skillbundle.Store.
type Store struct {
	mu   sync.RWMutex
	data map[string][]byte
}

var _ skillbundle.Store = (*Store)(nil)

// New constructs an empty in-memory store.
func New() *Store { return &Store{data: map[string][]byte{}} }

func (s *Store) Put(_ context.Context, digest string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.data[digest] = cp
	return nil
}

func (s *Store) Get(_ context.Context, digest string) ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data[digest]
	if !ok {
		return nil, skillbundle.ErrNotFound
	}
	cp := make([]byte, len(b))
	copy(cp, b)
	return cp, nil
}

func (s *Store) Has(_ context.Context, digest string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[digest]
	return ok, nil
}
