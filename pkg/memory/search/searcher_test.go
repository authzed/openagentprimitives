package search_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory"
	"github.com/authzed/openagentprimitives/pkg/memory/search"
)

// testLogger returns a logr.Logger that captures every Info line so tests
// can assert on the message text without a real logging backend.
func testLogger(t *testing.T) (logr.Logger, *[]string) {
	t.Helper()
	var msgs []string
	l := funcr.New(func(prefix, args string) {
		msgs = append(msgs, prefix+" "+args)
	}, funcr.Options{})
	return l, &msgs
}

type fakeProvider struct {
	name    string
	caps    memory.SearchCapabilities
	results memory.SearchResult
	err     error
}

func (p *fakeProvider) Name() string                                  { return p.name }
func (p *fakeProvider) SearchCapabilities() memory.SearchCapabilities { return p.caps }
func (p *fakeProvider) Search(_ context.Context, _ memory.SearchRequest) (memory.SearchResult, error) {
	return p.results, p.err
}
func (p *fakeProvider) Index(_ context.Context, _ memory.Scope, _, _ string, _ []byte) error {
	return nil
}
func (p *fakeProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, _ string) error { return nil }
func (p *fakeProvider) DeleteScopeIndex(_ context.Context, _ memory.Scope) error         { return nil }

type fakeAuthz struct {
	filter func([]memory.Entry) []memory.Entry
}

func (a *fakeAuthz) AuthorizePut(_ context.Context, _ memory.Entry) error { return nil }
func (a *fakeAuthz) AuthorizeQuery(_ context.Context, entries []memory.Entry) ([]memory.Entry, error) {
	if a.filter != nil {
		return a.filter(entries), nil
	}
	return entries, nil
}
func (a *fakeAuthz) AuthorizeDelete(_ context.Context, _ memory.Scope, _, _ string) error { return nil }
func (a *fakeAuthz) CleanupScope(_ context.Context, _ memory.Scope) error                 { return nil }

func TestCompositeSearcher_FansOutToProviders(t *testing.T) {
	p1 := &fakeProvider{
		name: "pg",
		results: memory.SearchResult{Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
		}},
	}
	p2 := &fakeProvider{
		name: "graphiti",
		results: memory.SearchResult{Entries: []memory.ScoredEntry{
			{Entry: entry("entity", "ent-1"), Score: 0.8, Source: "graphiti"},
		}},
	}
	s := search.New(search.WithProviders(p1, p2))
	res, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Text:   "test",
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 2)
	assert.Len(t, res.PerProvider, 2)
	assert.Contains(t, res.PerProvider, "pg")
	assert.Contains(t, res.PerProvider, "graphiti")
}

func TestCompositeSearcher_ProviderErrorDoesNotFail(t *testing.T) {
	good := &fakeProvider{
		name: "pg",
		results: memory.SearchResult{Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
		}},
	}
	bad := &fakeProvider{name: "graphiti", err: errors.New("sidecar down")}
	s := search.New(search.WithProviders(good, bad))

	res, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 1, "good provider's results should survive")
}

// TestCompositeSearcher_PartialFailureIsLogged pins the fix for a silent
// error drop: when some providers fail and others succeed, Search must
// still return the OK providers' results (that behavior is unchanged) AND
// log the failed providers — previously errs was discarded outright once
// perProvider was non-empty, so a degraded search (e.g. graphiti sidecar
// down) was invisible in logs.
func TestCompositeSearcher_PartialFailureIsLogged(t *testing.T) {
	good := &fakeProvider{
		name: "pg",
		results: memory.SearchResult{Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-1"), Score: 0.9, Source: "pg"},
		}},
	}
	bad := &fakeProvider{name: "graphiti", err: errors.New("sidecar down")}
	logger, msgs := testLogger(t)
	s := search.New(search.WithProviders(good, bad), search.WithLogger(logger))

	res, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, res.Entries, 1, "good provider's results should survive")

	require.NotEmpty(t, *msgs, "the failed provider must be logged, not silently dropped")
	found := false
	for _, m := range *msgs {
		if strings.Contains(m, "some providers failed") && strings.Contains(m, "graphiti") && strings.Contains(m, "sidecar down") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected a log line naming the failed provider and its error, got: %v", *msgs)
}

func TestCompositeSearcher_AllProvidersError(t *testing.T) {
	bad1 := &fakeProvider{name: "pg", err: errors.New("pg down")}
	bad2 := &fakeProvider{name: "graphiti", err: errors.New("sidecar down")}
	s := search.New(search.WithProviders(bad1, bad2))

	_, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Limit:  10,
	})
	require.Error(t, err)
}

func TestCompositeSearcher_AuthzPostFilter(t *testing.T) {
	p := &fakeProvider{
		name: "pg",
		results: memory.SearchResult{Entries: []memory.ScoredEntry{
			{Entry: entry("fact", "fact-allowed"), Score: 0.9, Source: "pg"},
			{Entry: entry("fact", "fact-denied"), Score: 0.8, Source: "pg"},
		}},
	}
	az := &fakeAuthz{filter: func(entries []memory.Entry) []memory.Entry {
		var out []memory.Entry
		for _, e := range entries {
			if e.ID == "fact-allowed" {
				out = append(out, e)
			}
		}
		return out
	}}
	s := search.New(search.WithProviders(p), search.WithAuthorizer(az))

	ctx := memory.WithCaller(memory.WithSystemApproval(context.Background(), "test"), "user-1")
	res, err := s.Search(ctx, memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
		Limit:  10,
	})
	require.NoError(t, err)
	require.Len(t, res.Entries, 1)
	assert.Equal(t, "fact-allowed", res.Entries[0].Entry.ID)
}

func TestCompositeSearcher_NoProviders(t *testing.T) {
	s := search.New()
	_, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
	})
	require.ErrorIs(t, err, memory.ErrNoSearchProviders)
}

func TestCompositeSearcher_DefaultLimit(t *testing.T) {
	entries := make([]memory.ScoredEntry, 30)
	for i := range entries {
		entries[i] = memory.ScoredEntry{
			Entry:  entry("fact", "fact-"+string(rune('a'+i))),
			Score:  float64(30-i) / 30.0,
			Source: "pg",
		}
	}
	p := &fakeProvider{name: "pg", results: memory.SearchResult{Entries: entries}}
	s := search.New(search.WithProviders(p))

	res, err := s.Search(memory.WithSystemApproval(context.Background(), "test"), memory.SearchRequest{
		Scopes: []memory.Scope{{Kind: "session", ID: "ns/a"}},
	})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(res.Entries), 20, "default limit is 20")
}
