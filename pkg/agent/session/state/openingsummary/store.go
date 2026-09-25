// Package openingsummary holds a triggered session's agent-authored enrichment
// body — the text appended to the pinned opening message. Last-write-wins,
// replayable across resume via a system_note.
package openingsummary

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
)

const KindName = "openingsummary"
const noteVersion = 1

type Store struct {
	mu   sync.RWMutex
	body string
	deps state.Deps
}

func NewStore(deps state.Deps) *Store { return &Store{deps: deps} }

func (*Store) Kind() string { return KindName }

// SetBody replaces the enrichment body. A no-change write persists nothing.
func (s *Store) SetBody(ctx context.Context, body string) error {
	s.mu.Lock()
	unchanged := s.body == body
	s.body = body
	s.mu.Unlock()
	if unchanged {
		return nil
	}
	return s.persistNote(ctx, body)
}

func (s *Store) Body() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.body
}

type noteData struct {
	Body string `json:"body"`
}

func (s *Store) persistNote(ctx context.Context, body string) error {
	if s.deps.AppendSystemNote == nil {
		return nil // no-op in fixtures / kubectl mode
	}
	raw, err := json.Marshal(noteData{Body: body})
	if err != nil {
		return fmt.Errorf("marshal opening-summary note: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("unmarshal opening-summary note: %w", err)
	}
	return s.deps.AppendSystemNote(ctx, map[string]any{
		"kind": KindName,
		"v":    noteVersion,
		"data": data,
	})
}

func (s *Store) ReplayNote(payload json.RawMessage) error {
	var d noteData
	if err := json.Unmarshal(payload, &d); err != nil {
		return fmt.Errorf("decode opening-summary note: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.body = d.Body
	return nil
}
