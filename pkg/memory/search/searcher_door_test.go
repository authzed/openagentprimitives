package search_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
)

// TestSearchDoor exercises the ReadMemory door added to
// CompositeSearcher.Search: a multi-scope search requires an approval for
// EVERY scope in the request, not just one. The fake provider returns no
// results — the door runs before any provider is invoked, so this test is
// self-contained (no Local.Put seeding needed).
func TestSearchDoor(t *testing.T) {
	p := &fakeProvider{name: "pg", results: memory.SearchResult{}}
	s := search.New(search.WithProviders(p))

	scopeA := memory.Scope{Kind: "session", ID: "nsA/sessA"}
	scopeB := memory.Scope{Kind: "session", ID: "nsB/sessB"}
	req := memory.SearchRequest{
		Scopes: []memory.Scope{scopeA, scopeB},
		Limit:  10,
	}

	t.Run("approval for only one of two scopes: ErrMissingApproval", func(t *testing.T) {
		ctx := memory.WithApproval(context.Background(),
			memory.ForBearerToken(memory.ReadMemory, scopeA.ID, "tok-1"))
		_, err := s.Search(ctx, req)
		require.ErrorIs(t, err, memory.ErrMissingApproval)
	})

	t.Run("system approval covers every scope: ok", func(t *testing.T) {
		ctx := memory.WithSystemApproval(context.Background(), "operator:test")
		_, err := s.Search(ctx, req)
		require.NoError(t, err)
	})

	t.Run("ReadMemory approval for both scopes: ok", func(t *testing.T) {
		ctx := memory.WithApproval(context.Background(),
			memory.ForBearerToken(memory.ReadMemory, scopeA.ID, "tok-1"),
			memory.ForBearerToken(memory.ReadMemory, scopeB.ID, "tok-2"))
		_, err := s.Search(ctx, req)
		require.NoError(t, err)
	})
}

// TestSearchDoor_EmptyScopes verifies an empty-scopes (cross-scope/all-sessions)
// search fails closed — the door must NOT be skipped on empty input — and is
// authorized only by a trusted system approval, never a bearer approval.
func TestSearchDoor_EmptyScopes(t *testing.T) {
	p := &fakeProvider{name: "pg", results: memory.SearchResult{}}
	s := search.New(search.WithProviders(p))
	req := memory.SearchRequest{Text: "q", Limit: 10} // no Scopes

	t.Run("no approval: ErrMissingApproval (not a silent all-sessions read)", func(t *testing.T) {
		_, err := s.Search(context.Background(), req)
		require.ErrorIs(t, err, memory.ErrMissingApproval)
	})

	t.Run("bearer approval does not authorize a cross-scope search", func(t *testing.T) {
		ctx := memory.WithApproval(context.Background(),
			memory.ForBearerToken(memory.ReadMemory, "nsA/sessA", "tok-1"))
		_, err := s.Search(ctx, req)
		require.ErrorIs(t, err, memory.ErrMissingApproval)
	})

	t.Run("system approval authorizes the admin cross-scope search: ok", func(t *testing.T) {
		ctx := memory.WithSystemApproval(context.Background(), "operator:admind")
		_, err := s.Search(ctx, req)
		require.NoError(t, err)
	})
}
