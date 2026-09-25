package inmem

import (
	"context"
	"sort"
	"sync"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// Backend is the in-process Backend implementation for the unified
// agent-memory framework. Every Kind — including the turn transcript —
// stores its entries here through the Local facade.
type Backend struct {
	mu      sync.RWMutex
	entries map[scopeKey]map[kindIDKey]*memory.Entry
	order   map[scopeKindKey][]string
}

type scopeKey struct{ kind, id string }
type kindIDKey struct{ kind, id string }
type scopeKindKey struct {
	scopeKind, scopeID, kindName string
}

// NewBackend constructs an empty in-process backend.
func NewBackend() *Backend {
	return &Backend{
		entries: map[scopeKey]map[kindIDKey]*memory.Entry{},
		order:   map[scopeKindKey][]string{},
	}
}

// Capabilities reports what this backend can answer. ContentSchemas and
// FieldRange are TRUE because matchesFieldEquals reproduces the SQL backends'
// semantics. The unit suite runs on this backend, so a capability it declines
// is one no unit test can exercise — declining FieldEquals once hid two
// accessors that were broken on both shipped backends.
func (*Backend) Capabilities() memory.Capabilities {
	return memory.Capabilities{
		ContentSchemas: true,
		FieldRange:     true,
		LinkTraversal:  1,
		ReverseLinks:   true,
		TagFilters:     true,
		TimeRange:      true,
	}
}

// Put stores a (defensive copy of) e under (e.Scope, e.Kind, e.ID).
// Last-writer-wins for the same key. When the entry is new and its Kind
// has a SoftCapPerScope, the oldest entry for (scope, kind) is evicted
// after the cap is exceeded.
func (b *Backend) Put(_ context.Context, e memory.Entry) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	sk := scopeKey{kind: e.Scope.Kind, id: e.Scope.ID}
	bucket, ok := b.entries[sk]
	if !ok {
		bucket = map[kindIDKey]*memory.Entry{}
		b.entries[sk] = bucket
	}
	cp := e
	kIK := kindIDKey{kind: e.Kind, id: e.ID}
	_, existed := bucket[kIK]
	bucket[kIK] = &cp

	if existed {
		return nil
	}

	// LookupKind is an O(1) map read; RegisteredKinds allocates a slice of
	// every registered Kind and sorts it, under this write lock, to answer
	// the same question about one of them.
	k, known := memory.LookupKind(e.Kind)
	if !known {
		return nil
	}
	if cap := k.Retention().SoftCapPerScope; cap > 0 {
		okey := scopeKindKey{e.Scope.Kind, e.Scope.ID, e.Kind}
		b.order[okey] = append(b.order[okey], e.ID)
		for len(b.order[okey]) > cap {
			drop := b.order[okey][0]
			b.order[okey] = b.order[okey][1:]
			delete(bucket, kindIDKey{kind: e.Kind, id: drop})
		}
	}
	return nil
}

// Get returns the entry at (scope, kind, id), or (zero, false, nil)
// when not found.
func (b *Backend) Get(_ context.Context, scope memory.Scope, kind, id string) (memory.Entry, bool, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket, ok := b.entries[scopeKey{kind: scope.Kind, id: scope.ID}]
	if !ok {
		return memory.Entry{}, false, nil
	}
	e, ok := bucket[kindIDKey{kind: kind, id: id}]
	if !ok {
		return memory.Entry{}, false, nil
	}
	return *e, true, nil
}

// Delete removes (scope, kind, id). Idempotent.
func (b *Backend) Delete(_ context.Context, scope memory.Scope, kind, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	bucket, ok := b.entries[scopeKey{kind: scope.Kind, id: scope.ID}]
	if !ok {
		return nil
	}
	delete(bucket, kindIDKey{kind: kind, id: id})

	okey := scopeKindKey{scope.Kind, scope.ID, kind}
	if list, ok := b.order[okey]; ok {
		for i, x := range list {
			if x == id {
				b.order[okey] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}
	return nil
}

// DeleteScope removes every entry under scope, along with the scope's
// per-Kind insertion-order bookkeeping. Idempotent — an unknown scope
// is a no-op.
func (b *Backend) DeleteScope(_ context.Context, scope memory.Scope) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, scopeKey{kind: scope.Kind, id: scope.ID})
	for okey := range b.order {
		if okey.scopeKind == scope.Kind && okey.scopeID == scope.ID {
			delete(b.order, okey)
		}
	}
	return nil
}

// Query filters the scope's bucket in order: Kinds → IDs → Tags → Since/Until →
// LinkedTo → LinkedFrom → FieldEquals → sort → Limit. FieldEquals is evaluated
// with the SQL backends' semantics (matchesFieldEquals), so nothing is dropped
// and DroppedPredicates stays empty.
func (b *Backend) Query(_ context.Context, q memory.Query) (memory.QueryResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	bucket, ok := b.entries[scopeKey{kind: q.Scope.Kind, id: q.Scope.ID}]
	if !ok {
		return memory.QueryResult{}, nil
	}

	kindSet := setOf(q.Kinds)
	idSet := setOf(q.IDs)

	var matched []memory.Entry
	for _, e := range bucket {
		if len(kindSet) > 0 {
			if _, ok := kindSet[e.Kind]; !ok {
				continue
			}
		}
		if len(idSet) > 0 {
			if _, ok := idSet[e.ID]; !ok {
				continue
			}
		}
		if !hasAllTags(e.Tags, q.Tags) {
			continue
		}
		if q.Since != nil && e.CreatedAt.Before(*q.Since) {
			continue
		}
		if q.Until != nil && !e.CreatedAt.Before(*q.Until) {
			continue
		}
		if !matchesLinkedTo(e.Links, q.LinkedTo) {
			continue
		}
		if !matchesLinkedFrom(e, bucket, q.LinkedFrom) {
			continue
		}
		if !matchesFieldEquals(e.Content, q.FieldEquals) {
			continue
		}
		matched = append(matched, *e)
	}

	// `matched` came out of a Go map, so its order is randomized per call. Sort
	// whenever the answer depends on order — an explicit OrderBy, or a Limit
	// about to throw entries away, since truncating a random walk returns an
	// arbitrary subset that differs every call and can never be paged through.
	// Default is newest-first, with entry ID breaking CreatedAt ties, the same
	// tiebreak QueryAllScopes uses.
	if q.OrderBy.Field == "createdAt" || q.Limit > 0 {
		desc := q.OrderBy.Desc || q.OrderBy.Field != "createdAt"
		sort.Slice(matched, func(i, j int) bool {
			if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
				if desc {
					return matched[i].CreatedAt.After(matched[j].CreatedAt)
				}
				return matched[i].CreatedAt.Before(matched[j].CreatedAt)
			}
			return matched[i].ID < matched[j].ID
		})
	}
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}

	return memory.QueryResult{Entries: matched}, nil
}

// QueryAllScopes scans every scope bucket of q.ScopeKind. The map scan is
// O(entries) — fine for the inmem backend's dev/test scale.
func (b *Backend) QueryAllScopes(_ context.Context, q memory.CrossScopeQuery) (memory.QueryResult, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	kindSet := setOf(q.Kinds)
	var matched []memory.Entry
	for sk, bucket := range b.entries {
		if sk.kind != q.ScopeKind {
			continue
		}
		for _, e := range bucket {
			if _, ok := kindSet[e.Kind]; !ok {
				continue
			}
			if q.Since != nil && e.CreatedAt.Before(*q.Since) {
				continue
			}
			if q.Until != nil && !e.CreatedAt.Before(*q.Until) {
				continue
			}
			matched = append(matched, *e)
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			if q.OrderDesc {
				return matched[i].CreatedAt.After(matched[j].CreatedAt)
			}
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		// Deterministic tiebreak so pagination never duplicates/skips.
		if matched[i].Scope.ID != matched[j].Scope.ID {
			return matched[i].Scope.ID < matched[j].Scope.ID
		}
		return matched[i].ID < matched[j].ID
	})
	if q.Offset > 0 {
		if q.Offset >= len(matched) {
			matched = nil
		} else {
			matched = matched[q.Offset:]
		}
	}
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}
	return memory.QueryResult{Entries: matched}, nil
}

func setOf(xs []string) map[string]struct{} {
	if len(xs) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func hasAllTags(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	set := setOf(have)
	for _, w := range want {
		if _, ok := set[w]; !ok {
			return false
		}
	}
	return true
}

func matchesLinkedTo(links []memory.Link, filters []memory.LinkFilter) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		hit := false
		for _, l := range links {
			if f.Relation != "" && l.Relation != f.Relation {
				continue
			}
			if l.Kind == f.Kind && l.ID == f.ID {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

func matchesLinkedFrom(e *memory.Entry, bucket map[kindIDKey]*memory.Entry, filters []memory.LinkFilter) bool {
	if len(filters) == 0 {
		return true
	}
	for _, f := range filters {
		hit := false
		for _, other := range bucket {
			for _, l := range other.Links {
				if f.Relation != "" && l.Relation != f.Relation {
					continue
				}
				if l.Kind == e.Kind && l.ID == e.ID && other.Kind == f.Kind && other.ID == f.ID {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if !hit {
			return false
		}
	}
	return true
}

// Status returns Live when the backend holds any entries for scope, Unknown
// otherwise. It never returns Archived: time-based archival is not implemented
// in any backend.
func (b *Backend) Status(_ context.Context, scope memory.Scope) (memory.ScopeStatus, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	bucket, ok := b.entries[scopeKey{kind: scope.Kind, id: scope.ID}]
	if ok && len(bucket) > 0 {
		return memory.ScopeStatus{State: memory.StatusLive}, nil
	}
	return memory.ScopeStatus{State: memory.StatusUnknown}, nil
}
