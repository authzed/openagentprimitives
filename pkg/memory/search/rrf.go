package search

import (
	"sort"
	"strings"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// RRFRanker implements memory.Ranker using Reciprocal Rank Fusion (Cormack et al., 2009).
// Entries appearing in multiple providers are boosted. K defaults to 60 when zero.
type RRFRanker struct {
	K int
}

type entryKey struct {
	scopeKind, scopeID, kind, id string
}

func keyOf(e memory.Entry) entryKey {
	return entryKey{e.Scope.Kind, e.Scope.ID, e.Kind, e.ID}
}

type scored struct {
	key       entryKey
	entry     memory.ScoredEntry
	sourceSet map[string]struct{}
	rrf       float64
}

// Rank merges results from multiple providers using RRF, deduplicates by
// (Scope, Kind, ID), normalises scores to [0,1], and truncates to limit.
func (r *RRFRanker) Rank(results map[string]memory.SearchResult, limit int) []memory.ScoredEntry {
	k := r.K
	if k == 0 {
		k = 60
	}

	acc := map[entryKey]*scored{}
	for _, sr := range results {
		for rank, se := range sr.Entries {
			ek := keyOf(se.Entry)
			s, ok := acc[ek]
			if !ok {
				s = &scored{key: ek, entry: se, sourceSet: map[string]struct{}{}}
				acc[ek] = s
			}
			s.sourceSet[se.Source] = struct{}{}
			s.rrf += 1.0 / float64(k+rank+1)
		}
	}

	flat := make([]*scored, 0, len(acc))
	for _, s := range acc {
		flat = append(flat, s)
	}
	sort.Slice(flat, func(i, j int) bool { return flat[i].rrf > flat[j].rrf })

	if limit > 0 && len(flat) > limit {
		flat = flat[:limit]
	}

	var maxRRF float64
	if len(flat) > 0 {
		maxRRF = flat[0].rrf
	}

	out := make([]memory.ScoredEntry, len(flat))
	for i, s := range flat {
		out[i] = s.entry
		sources := make([]string, 0, len(s.sourceSet))
		for src := range s.sourceSet {
			sources = append(sources, src)
		}
		sort.Strings(sources)
		out[i].Source = strings.Join(sources, "+")
		if maxRRF > 0 {
			out[i].Score = s.rrf / maxRRF
		}
	}
	return out
}
