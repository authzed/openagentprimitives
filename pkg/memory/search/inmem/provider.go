package inmem

import (
	"context"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

// InmemSearchProvider answers the STRUCTURED half of a search by delegating to
// the Backend's own Query. It is what keeps a text-less request from returning
// nothing: the lexical providers bail on empty Text, so without this one a
// tags-or-time-only search would have no answering provider at all.
//
// Every entry scores a flat 1.0 — this provider ranks nothing, it only
// contributes candidates for RRF to fuse against the scoring providers.
type InmemSearchProvider struct {
	backend memory.Backend
}

var _ memory.SearchProvider = (*InmemSearchProvider)(nil)

func New(backend memory.Backend) *InmemSearchProvider {
	return &InmemSearchProvider{backend: backend}
}

func (*InmemSearchProvider) Name() string { return "inmem" }

// SearchCapabilities claims only the filters Backend.Query honors directly.
// TextSearch and VectorSearch stay false: this provider ignores req.Text
// entirely rather than pretending to rank by it.
func (*InmemSearchProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{
		TagFilters: true,
		TimeRange:  true,
	}
}

func (p *InmemSearchProvider) Search(ctx context.Context, req memory.SearchRequest) (memory.SearchResult, error) {
	var allEntries []memory.ScoredEntry
	for _, scope := range req.Scopes {
		q := memory.Query{
			Scope: scope,
			Kinds: req.Kinds,
			Tags:  req.Tags,
			Since: req.Since,
			Until: req.Until,
			Limit: req.Limit,
		}
		res, err := p.backend.Query(ctx, q)
		if err != nil {
			return memory.SearchResult{}, err
		}
		for _, e := range res.Entries {
			allEntries = append(allEntries, memory.ScoredEntry{
				Entry:  e,
				Score:  1.0,
				Source: "inmem",
			})
		}
	}
	if req.Limit > 0 && len(allEntries) > req.Limit {
		allEntries = allEntries[:req.Limit]
	}
	return memory.SearchResult{Entries: allEntries}, nil
}

// Index, DeleteIndex and DeleteScopeIndex are no-ops: this provider reads the
// Backend directly, so it has no index of its own to keep in step.
func (*InmemSearchProvider) Index(_ context.Context, _ memory.Scope, _, _ string, _ []byte) error {
	return nil
}

func (*InmemSearchProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, _ string) error {
	return nil
}

func (*InmemSearchProvider) DeleteScopeIndex(_ context.Context, _ memory.Scope) error {
	return nil
}
