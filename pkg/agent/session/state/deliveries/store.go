// Package deliveries is the session-state Kind recording WHICH artifact
// renders actually reached a user.
//
// A render being Ready says the bytes exist; it says nothing about whether
// anyone saw them. Only respond_to_user's `attached` list carries an artifact
// to the channel, and until this store existed that fact lived nowhere: a
// session could render a report, tell the user it was attached, attach nothing,
// and finish clean. The completion requirement `artifact-delivered` is the
// reader; respond_to_user is the sole writer.
//
// It is a state.Kind rather than a field on the runner so it replays: a resumed
// session rebuilds the delivered set from its own system_note trail instead of
// re-reporting every already-delivered artifact as outstanding.
package deliveries

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/agent/session/state"
)

// noteVersion is the v field on the wrapped system_note for deliveries. Bump
// and add a migration in ReplayNote when the data shape changes.
//
// v2 added the parallel `artifacts` list. It is additive: a v1 note simply
// carries no artifact ids, which ReplayNote reads as "delivered, but not
// linkable" — the same state a render with no artifact-id label produces.
const noteVersion = 2

// Item is one artifact render that reached a user.
//
// Both halves name the same delivery from the two directions the system asks
// about it: RenderName is the ArtifactRender CR name the completion
// requirement lists back from the API server, and ArtifactID is the logical
// artifact a durable view link names. Recording only one of them would leave
// the other question unanswerable.
type Item struct {
	// RenderName is the ArtifactRender CR name respond_to_user resolved every
	// handle down to before publishing (channelevents.AttachmentRef.RenderName).
	RenderName string
	// ArtifactID is the logical artifact the render belongs to
	// (artifacts.LabelArtifactID on the CR). Empty when the CR carries no such
	// label — the delivery still counts, it just cannot be linked to.
	ArtifactID string
}

// Store is the per-session set of delivered ArtifactRender CR names, plus the
// order they were delivered in.
//
// The CR NAME is the set's key because that is what respond_to_user resolves
// every handle down to before it publishes, and what the completion
// requirement lists back from the API server. Recording the agent's handle
// instead would compare two different namings of the same thing — a tagged
// revision handle and a render name — and never match.
type Store struct {
	mu        sync.RWMutex
	delivered map[string]bool
	// artifactOrder is the logical artifact ids in delivery order, one entry
	// per newly-delivered render that carried one. A slice rather than a
	// "latest" scalar so replay reconstructs the same ordering from the note
	// trail instead of depending on which note happened to be applied last.
	artifactOrder []string

	deps state.Deps
}

// NewStore is the factory invoked by the Kind.
func NewStore(deps state.Deps) *Store {
	return &Store{delivered: map[string]bool{}, deps: deps}
}

// Kind implements tool.StateStore.
func (*Store) Kind() string { return "deliveries" }

// Record marks each item's render delivered and persists the addition as a
// system_note so a resumed session replays it.
//
// Called AFTER the outbound envelope is on the wire: recording a delivery that
// then failed to publish would tell the completion gate a user saw something
// they did not. Renders already recorded are dropped, so a re-delivery of the
// same artifact writes no note — and does not move the "most recently
// delivered" pointer backwards onto an artifact the session has moved past.
func (s *Store) Record(ctx context.Context, items ...Item) error {
	// One critical section for BOTH mutations. Marking a render delivered and
	// appending its artifact to the order are one fact; splitting them would
	// let two concurrent Records interleave into an order that no longer
	// matches the order the notes were written in, and replay would then
	// disagree with the live store about which delivery was last.
	s.mu.Lock()
	var added []Item
	for _, it := range items {
		if it.RenderName == "" || s.delivered[it.RenderName] {
			continue
		}
		s.delivered[it.RenderName] = true
		added = append(added, it)
	}
	if len(added) == 0 {
		s.mu.Unlock()
		return nil
	}
	// Deterministic note payload. Sorting by render name means one call
	// delivering several artifacts records them in a stable order; ACROSS calls
	// the notes stay in wall-clock order, which is what carries "most recent".
	sort.Slice(added, func(i, j int) bool { return added[i].RenderName < added[j].RenderName })

	renders := make([]string, 0, len(added))
	artifacts := make([]string, 0, len(added))
	for _, it := range added {
		renders = append(renders, it.RenderName)
		if it.ArtifactID != "" {
			artifacts = append(artifacts, it.ArtifactID)
		}
	}
	s.artifactOrder = append(s.artifactOrder, artifacts...)
	s.mu.Unlock()

	// Outside the lock: persistNote does I/O, and a lock held across it would
	// block every concurrent Delivered/LastArtifactID read for its duration.
	return s.persistNote(ctx, renders, artifacts)
}

// Delivered reports whether a render name has been delivered.
func (s *Store) Delivered(renderName string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.delivered[renderName]
}

// All returns every delivered render name, sorted. Test / debug helper.
func (s *Store) All() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.delivered))
	for n := range s.delivered {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LastArtifactID returns the logical artifact id of the most recent delivery
// that carried one, or "" when this session has delivered nothing linkable.
//
// This is what a caller naming "the session's result" uses — the trigger-status
// tool's default details link. The most recent is the defensible pick: it is
// the last thing the agent put in front of a person, and therefore what the
// judgement it is about to publish refers to.
func (s *Store) LastArtifactID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.artifactOrder) == 0 {
		return ""
	}
	return s.artifactOrder[len(s.artifactOrder)-1]
}

// noteData is this Kind's system_note payload shape.
type noteData struct {
	// Renders are the render names this note adds. Additive: a note is a
	// delta, never the full set, because the set only ever grows and a
	// full-state note would rewrite the whole list on every reply.
	Renders []string `json:"renders"`
	// Artifacts are the logical artifact ids for those renders that carried
	// one, in the same delivery order. Not positionally aligned with Renders:
	// a render with no artifact-id label contributes to Renders only. Absent
	// on notes written before deliveries recorded artifact ids.
	Artifacts []string `json:"artifacts,omitempty"`
}

func (s *Store) persistNote(ctx context.Context, renders, artifacts []string) error {
	if s.deps.AppendSystemNote == nil {
		return nil // no-op in test fixtures / kubectl mode
	}
	raw, err := json.Marshal(noteData{Renders: renders, Artifacts: artifacts})
	if err != nil {
		return fmt.Errorf("marshal deliveries note: %w", err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("unmarshal deliveries note: %w", err)
	}
	return s.deps.AppendSystemNote(ctx, map[string]any{
		"kind": "deliveries",
		"v":    noteVersion,
		"data": data,
	})
}

// ReplayNote rebuilds the delivered set from a previously-emitted note.
//
// Notes arrive in the order they were written, so appending each note's
// artifact ids reconstructs the same delivery order the live session had — and
// with it the same answer from LastArtifactID.
func (s *Store) ReplayNote(payload json.RawMessage) error {
	var d noteData
	if err := json.Unmarshal(payload, &d); err != nil {
		return fmt.Errorf("decode deliveries note: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range d.Renders {
		if n != "" {
			s.delivered[n] = true
		}
	}
	for _, a := range d.Artifacts {
		if a != "" {
			s.artifactOrder = append(s.artifactOrder, a)
		}
	}
	return nil
}
