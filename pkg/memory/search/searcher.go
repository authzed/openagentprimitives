package search

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/go-logr/logr"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

type CompositeSearcher struct {
	providers []memory.SearchProvider
	ranker    memory.Ranker
	authz     memory.Authorizer
	logger    logr.Logger
}

type Option func(*CompositeSearcher)

func WithProviders(ps ...memory.SearchProvider) Option {
	return func(s *CompositeSearcher) { s.providers = append(s.providers, ps...) }
}

func WithRanker(r memory.Ranker) Option {
	return func(s *CompositeSearcher) { s.ranker = r }
}

func WithAuthorizer(a memory.Authorizer) Option {
	return func(s *CompositeSearcher) { s.authz = a }
}

// WithLogger sets the logger used to report partial provider-search failures.
// Defaults to the discard logger, so CompositeSearcher is safe to construct
// without one.
func WithLogger(l logr.Logger) Option {
	return func(s *CompositeSearcher) { s.logger = l }
}

func New(opts ...Option) *CompositeSearcher {
	s := &CompositeSearcher{
		ranker: &RRFRanker{K: 60},
		logger: logr.Discard(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Search fans a SearchRequest out to every configured provider, ranks the
// merged results, and post-filters through the Authorizer.
//
// The door: EVERY scope in req.Scopes must carry an approval for (ReadMemory,
// scope.ID) or Search returns ErrMissingApproval before any provider runs — one
// approved scope does not authorize a search spanning an unapproved one.
//
// An EMPTY req.Scopes means a cross-scope (all-sessions) search, which must fail
// CLOSED rather than skip the loop: EnsureApproval with an empty resource is
// satisfied only by a system approval. The HTTP path always forces a single URL
// scope, so this is reachable only by in-process admin callers that mint one.
func (s *CompositeSearcher) Search(ctx context.Context, req memory.SearchRequest) (memory.MergedSearchResult, error) {
	if len(req.Scopes) == 0 {
		if err := memory.EnsureApproval(ctx, memory.ReadMemory, ""); err != nil {
			return memory.MergedSearchResult{}, err
		}
	}
	for _, sc := range req.Scopes {
		if err := memory.EnsureApproval(ctx, memory.ReadMemory, sc.ID); err != nil {
			return memory.MergedSearchResult{}, err
		}
	}

	if len(s.providers) == 0 {
		return memory.MergedSearchResult{}, memory.ErrNoSearchProviders
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 20
	}

	type provResult struct {
		name string
		res  memory.SearchResult
		err  error
	}

	// Ask every provider for one hit past the limit. Each provider caps its own
	// answer at SearchRequest.Limit, so without the probe a lone provider can
	// never hand back more rows than were asked for and the merged pool could
	// not distinguish "20 matched" from "20 shown of many" — the same ambiguity
	// QueryResult.Truncated resolves on the unranked side.
	probeReq := req
	probeReq.Limit = memory.ProbeLimit(limit)

	var wg sync.WaitGroup
	ch := make(chan provResult, len(s.providers))
	for _, p := range s.providers {
		wg.Add(1)
		go func(p memory.SearchProvider) {
			defer wg.Done()
			res, err := p.Search(ctx, probeReq)
			ch <- provResult{name: p.Name(), res: res, err: err}
		}(p)
	}
	wg.Wait()
	close(ch)

	perProvider := make(map[string]memory.SearchResult)
	var errs []error
	for pr := range ch {
		if pr.err != nil {
			errs = append(errs, fmt.Errorf("provider %s: %w", pr.name, pr.err))
			continue
		}
		perProvider[pr.name] = pr.res
	}

	if len(perProvider) == 0 {
		return memory.MergedSearchResult{}, errors.Join(errs...)
	}
	if len(errs) > 0 {
		// Degraded search is intended, but never SILENT: name how many
		// providers failed so an operator can locate them.
		s.logger.Info("composite search: some providers failed; returning partial results",
			"failedProviders", len(errs), "okProviders", len(perProvider), "err", errors.Join(errs...).Error())
	}

	// Count the distinct hits the providers actually found, BEFORE ranking
	// discards any: more of them than the caller's limit means the limit, not
	// the data, is what ends the page. Dedup across providers is what keeps two
	// providers echoing the same hits from reading as a fuller pool than it is.
	distinct := make(map[entryKey]struct{})
	for _, sr := range perProvider {
		for _, se := range sr.Entries {
			distinct[keyOf(se.Entry)] = struct{}{}
		}
	}
	truncated := len(distinct) > limit

	// Rank to 2× limit, then authorize, then truncate. Truncating first would
	// let the authz filter eat the page and return fewer hits than the caller
	// asked for while matches sat just past the cut.
	ranked := s.ranker.Rank(perProvider, limit*2)

	if s.authz != nil {
		if _, ok := memory.CallerFrom(ctx); ok {
			entries := make([]memory.Entry, len(ranked))
			for i, se := range ranked {
				entries[i] = se.Entry
			}
			allowed, err := s.authz.AuthorizeQuery(ctx, entries)
			if err != nil {
				return memory.MergedSearchResult{}, err
			}
			allowedSet := make(map[string]struct{}, len(allowed))
			for _, e := range allowed {
				allowedSet[e.Scope.Kind+"/"+e.Scope.ID+"/"+e.Kind+"/"+e.ID] = struct{}{}
			}
			filtered := make([]memory.ScoredEntry, 0, len(allowed))
			for _, se := range ranked {
				k := se.Entry.Scope.Kind + "/" + se.Entry.Scope.ID + "/" + se.Entry.Kind + "/" + se.Entry.ID
				if _, ok := allowedSet[k]; ok {
					filtered = append(filtered, se)
				}
			}
			ranked = filtered
		}
	}

	if len(ranked) > limit {
		ranked = ranked[:limit]
	}

	return memory.MergedSearchResult{
		Entries:     ranked,
		PerProvider: perProvider,
		Truncated:   truncated,
	}, nil
}
