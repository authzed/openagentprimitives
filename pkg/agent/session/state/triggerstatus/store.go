// Package triggerstatus is the session-state Kind recording whether the EVENT
// that started this session has been answered on its own status surface.
//
// A session bound to a trigger with a status surface — a pull request's GitHub
// check run today — owes that surface an answer. Whether it has given one lived
// nowhere: a review agent could deliver its findings to the thread, end the
// round, and leave the pull request showing the work as still in progress
// forever. The completion requirement `trigger-status-concluded` is the reader;
// the two trigger-status meta tools are the sole writers.
//
// # Why a recorded fact rather than a live read
//
// channelkinds.TriggerSurface has no read-only query. Claim reports whether the
// trigger already carries an answer, but it also OPENS a claim when none
// exists — so asking the provider "was this concluded?" at agent_work_complete
// time would put a pull request into "in progress" as a side effect of a
// completion check, which is the exact symptom the requirement exists to
// prevent. The tools know the answer at the moment they get it; this is where
// they leave it.
//
// It is a state.Kind rather than a field on the runner so it replays: a resumed
// session must not demand an answer it already delivered.
package triggerstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
	"github.com/authzed/openagentprimitives/pkg/channels/channelkinds"
)

// KindName is the system_note discriminator and the registry key.
const KindName = "triggerstatus"

// noteVersion is the v field on the wrapped system_note. Bump it and add a
// migration in ReplayNote when the data shape changes.
const noteVersion = 1

// Store is one session's record of the answer its trigger carries.
//
// A single outcome, not a set: a trigger has exactly one status surface and
// that surface holds exactly one answer at a time. A second conclusion replaces
// the first on the provider, so it replaces it here too.
type Store struct {
	mu      sync.RWMutex
	outcome channelkinds.TriggerOutcome

	deps state.Deps
}

// NewStore is the factory invoked by the Kind.
func NewStore(deps state.Deps) *Store { return &Store{deps: deps} }

// Kind implements tool.StateStore.
func (*Store) Kind() string { return KindName }

// RecordConcluded marks this session's trigger answered with o and persists the
// fact as a system_note so a resumed session replays it.
//
// Called only AFTER the answer is actually on the surface — because this
// session's conclude call succeeded, or because a claim reported the surface
// already carried one. Recording an answer that never landed would tell the
// completion gate a pull request was answered when it was not, which is worse
// than the gap it closes.
//
// The outcome is validated through the seam's own parser, so "concluded with
// nothing" is not a state this store can be put into: an empty or invented
// outcome would satisfy the gate without any answer existing.
func (s *Store) RecordConcluded(ctx context.Context, o channelkinds.TriggerOutcome) error {
	if _, err := channelkinds.ParseTriggerOutcome(string(o)); err != nil {
		return fmt.Errorf("record trigger conclusion: %w", err)
	}
	s.mu.Lock()
	unchanged := s.outcome == o
	s.outcome = o
	s.mu.Unlock()

	if unchanged {
		return nil
	}
	return s.persistNote(ctx, o)
}

// Concluded reports the answer this session's trigger carries, and whether it
// carries one at all. The outcome is returned alongside the bool so a reader
// can name the verdict rather than only its existence.
func (s *Store) Concluded() (channelkinds.TriggerOutcome, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.outcome, s.outcome != ""
}

// noteData is this Kind's system_note payload shape.
type noteData struct {
	// Outcome is the answer the trigger carries. Full state rather than a
	// delta: there is only ever one, and the newest note is the whole truth.
	Outcome string `json:"outcome"`
}

func (s *Store) persistNote(ctx context.Context, o channelkinds.TriggerOutcome) error {
	if s.deps.AppendSystemNote == nil {
		return nil // no-op in test fixtures / kubectl mode
	}
	raw, err := json.Marshal(noteData{Outcome: string(o)})
	if err != nil {
		return fmt.Errorf("marshal trigger-status note: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("unmarshal trigger-status note: %w", err)
	}
	return s.deps.AppendSystemNote(ctx, map[string]any{
		"kind": KindName,
		"v":    noteVersion,
		"data": data,
	})
}

// ReplayNote rebuilds the recorded answer from a previously-emitted note.
//
// It accepts any non-empty outcome, unlike RecordConcluded, which validates
// against the closed set. The note was written by a build that had already
// validated it, and refusing a value a future build added would break resume on
// a downgrade for a fact that is only ever reported, never dispatched on. An
// EMPTY outcome is still refused: that would silently leave the session
// believing its trigger is unanswered when the log says otherwise.
func (s *Store) ReplayNote(payload json.RawMessage) error {
	var d noteData
	if err := json.Unmarshal(payload, &d); err != nil {
		return fmt.Errorf("decode trigger-status note: %w", err)
	}
	if d.Outcome == "" {
		return fmt.Errorf("trigger-status note carries no outcome")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcome = channelkinds.TriggerOutcome(d.Outcome)
	return nil
}
