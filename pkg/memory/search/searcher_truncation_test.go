package search_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
)

// cappingProvider models what every real SearchProvider does with
// SearchRequest.Limit: it caps its own answer at the requested count. That cap
// is why the composite cannot infer truncation from the merged pool alone — a
// lone provider can never hand back more rows than were asked for — and so it
// is the behaviour the probe has to be measured against.
type cappingProvider struct {
	name string
	pool []memory.ScoredEntry

	mu       sync.Mutex
	gotLimit int // the Limit this provider was actually asked for
}

func (p *cappingProvider) Name() string { return p.name }
func (p *cappingProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{}
}

func (p *cappingProvider) Search(_ context.Context, req memory.SearchRequest) (memory.SearchResult, error) {
	p.mu.Lock()
	p.gotLimit = req.Limit
	p.mu.Unlock()
	out := p.pool
	if req.Limit > 0 && len(out) > req.Limit {
		out = out[:req.Limit]
	}
	return memory.SearchResult{Entries: out}, nil
}

func (p *cappingProvider) Index(_ context.Context, _ memory.Scope, _, _ string, _ []byte) error {
	return nil
}
func (p *cappingProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, _ string) error {
	return nil
}
func (p *cappingProvider) DeleteScopeIndex(_ context.Context, _ memory.Scope) error { return nil }

func (p *cappingProvider) askedFor() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.gotLimit
}

// scoredPool builds n distinct hits with ids sharing prefix.
func scoredPool(prefix string, n int) []memory.ScoredEntry {
	out := make([]memory.ScoredEntry, n)
	for i := range out {
		out[i] = memory.ScoredEntry{Entry: entry("label", fmt.Sprintf("%s-%03d", prefix, i)), Score: 1, Source: prefix}
	}
	return out
}

// TestCompositeSearcherTruncation pins the ranked half of the same defect: a
// top-N page that stopped because N ran out must say so, and one that ran out
// of matches must NOT.
func TestCompositeSearcherTruncation(t *testing.T) {
	ctx := memory.WithSystemApproval(context.Background(), "operator:test")
	scope := memory.Scope{Kind: "session", ID: "ns/sess"}

	t.Run("one provider with more matches than the limit: Truncated=true, page capped at the limit", func(t *testing.T) {
		p := &cappingProvider{name: "pg", pool: scoredPool("pg", 5)}
		s := search.New(search.WithProviders(p))
		res, err := s.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Limit: 3})
		require.NoError(t, err, "Search")
		assert.Equal(t, 4, p.askedFor(), "the provider must be asked for one row past the limit")
		assert.Len(t, res.Entries, 3, "the probe row must never reach the caller")
		assert.True(t, res.Truncated)
	})

	t.Run("one provider with exactly the limit: complete answer, Truncated=false", func(t *testing.T) {
		p := &cappingProvider{name: "pg", pool: scoredPool("pg", 3)}
		s := search.New(search.WithProviders(p))
		res, err := s.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Limit: 3})
		require.NoError(t, err, "Search")
		assert.Len(t, res.Entries, 3)
		assert.False(t, res.Truncated, "a page that filled exactly is not a page that was cut")
	})

	t.Run("one provider with fewer than the limit: Truncated=false", func(t *testing.T) {
		p := &cappingProvider{name: "pg", pool: scoredPool("pg", 2)}
		s := search.New(search.WithProviders(p))
		res, err := s.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Limit: 3})
		require.NoError(t, err, "Search")
		assert.Len(t, res.Entries, 2)
		assert.False(t, res.Truncated)
	})

	t.Run("two providers returning the SAME hits: dedup must not invent a truncation", func(t *testing.T) {
		pool := scoredPool("shared", 3)
		p1 := &cappingProvider{name: "pg", pool: pool}
		p2 := &cappingProvider{name: "graphiti", pool: pool}
		s := search.New(search.WithProviders(p1, p2))
		res, err := s.Search(ctx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Limit: 3})
		require.NoError(t, err, "Search")
		assert.Len(t, res.Entries, 3)
		assert.False(t, res.Truncated, "6 raw rows over 3 distinct entries is a complete answer")
	})

	t.Run("authorization filtering does not erase the truncation: the store still held more", func(t *testing.T) {
		p := &cappingProvider{name: "pg", pool: scoredPool("pg", 5)}
		s := search.New(
			search.WithProviders(p),
			search.WithAuthorizer(&fakeAuthz{filter: func(es []memory.Entry) []memory.Entry { return es[:1] }}),
		)
		callerCtx := memory.WithCaller(ctx, "user:someone")
		res, err := s.Search(callerCtx, memory.SearchRequest{Scopes: []memory.Scope{scope}, Limit: 3})
		require.NoError(t, err, "Search")
		assert.Len(t, res.Entries, 1, "the authorizer kept one hit")
		assert.True(t, res.Truncated, "the limit still cut the read short, whatever the filter then removed")
	})
}
