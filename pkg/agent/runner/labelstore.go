// Package runner — LabelStore caches (resourceType, id) → friendly label
// tuples stamped from MCP tool responses via per-tool `labels` CEL.
// Scope: one store per Loop (== one runner Pod == one session).
//
// Threat-model note: labels are sourced from prior MCP tool outputs,
// which are attacker-controllable. The store is consulted ONLY by
// deterministic code paths (channel renderers); it never feeds any
// LLM.
package runner

import (
	"strings"
	"sync"
)

// DefaultLabelStoreCap is the soft cap applied when NewLabelStore
// receives cap ≤ 0. Tuned for the expected per-session volume of
// labeled entities (companies, owners, repos, etc.); on overflow the
// oldest entry is dropped FIFO. LRU would be overkill at this scale.
const DefaultLabelStoreCap = 4096

// LabelStore is a concurrency-safe, FIFO-bounded map of
// (resourceType, id) → label. The zero value is NOT usable —
// callers must construct via NewLabelStore.
type LabelStore struct {
	mu  sync.RWMutex
	m   map[string]string
	seq []string // insertion order; truncated FIFO on cap overflow
	cap int
}

// NewLabelStore returns a store with the given soft cap. A cap of 0
// or negative resolves to DefaultLabelStoreCap.
func NewLabelStore(cap int) *LabelStore {
	if cap <= 0 {
		cap = DefaultLabelStoreCap
	}
	return &LabelStore{
		m:   make(map[string]string, cap),
		seq: make([]string, 0, cap),
		cap: cap,
	}
}

// Put stamps (resourceType, id, label) into the store. On re-Put with
// an existing key, the stored label is overwritten in place and the
// entry's FIFO position is preserved — re-Put does NOT touch `seq`.
// When `len(m) >= cap` on a NEW key, the oldest entry (seq[0]) is
// evicted before the new entry is appended.
func (s *LabelStore) Put(resourceType, id, label string) {
	if resourceType == "" || id == "" || label == "" {
		return
	}
	k := resourceType + ":" + id
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.m[k]; exists {
		s.m[k] = label
		return
	}
	if len(s.m) >= s.cap && len(s.seq) > 0 {
		drop := s.seq[0]
		s.seq = s.seq[1:]
		delete(s.m, drop)
	}
	s.m[k] = label
	s.seq = append(s.seq, k)
}

// Get returns the cached label for (resourceType, id) and ok=true on
// hit; ("", false) on miss.
func (s *LabelStore) Get(resourceType, id string) (string, bool) {
	if resourceType == "" || id == "" {
		return "", false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[resourceType+":"+id]
	return v, ok
}

// SnapshotForArgs returns the bare-id-keyed subset of the store
// relevant to one approval request: the primary (resourceType, id)
// pair if present, plus any string value in argsMap (walked
// recursively through maps + slices) that the store has a label for
// under ANY resource type. On id-collision across resource types,
// the primary's label wins; other types' duplicates are dropped.
//
// Return-key shape is the bare id (not "type:id") because the
// channel substitution layer string-replaces against the rendered
// approval text, which contains bare ids.
func (s *LabelStore) SnapshotForArgs(argsMap map[string]any, primaryType, primaryID string) map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := map[string]string{}
	// 1. Primary first so it wins on later collisions.
	if primaryType != "" && primaryID != "" {
		if v, ok := s.m[primaryType+":"+primaryID]; ok {
			out[primaryID] = v
		}
	}
	// 2. Walk args; for each string value, look it up across all
	// resource types. Skip if it collides with the primary's id.
	idsInArgs := collectStringValues(argsMap)
	for id := range idsInArgs {
		if id == primaryID {
			continue
		}
		// Scan known keys for any type matching this id.
		for k, v := range s.m {
			// k is "<type>:<id>"; pull the suffix after the LAST ":"
			// so types containing colons (none today, but defensive)
			// don't trip the split.
			colon := strings.LastIndexByte(k, ':')
			if colon < 0 || k[colon+1:] != id {
				continue
			}
			out[id] = v
			break
		}
	}
	return out
}

// collectStringValues walks v (maps + slices) and returns the set of
// distinct string leaf values. Non-string leaves (numbers, bools,
// nulls) are ignored.
func collectStringValues(v any) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			if t != "" {
				out[t] = struct{}{}
			}
		case map[string]any:
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(v)
	return out
}
