package memory

import (
	"context"
	"errors"
	"time"
)

var ErrNoSearchProviders = errors.New("memory: no search providers configured")

// SearchRequest is one ranked-retrieval query, fanned out to every provider.
type SearchRequest struct {
	// Scopes to search; every one must be separately authorized.
	Scopes []Scope `json:"scopes"`
	// Text is the free-text / vector query; empty makes this structured-only.
	Text string `json:"text,omitempty"`
	// Kinds to restrict to; empty means all kinds.
	Kinds []string `json:"kinds,omitempty"`
	// Tags an entry must ALL carry to match.
	Tags []string `json:"tags,omitempty"`
	// Fields are content predicates; providers lacking FieldFilters drop them.
	Fields []FieldFilter `json:"fields,omitempty"`
	// Since bounds CreatedAt below; nil means unbounded.
	Since *time.Time `json:"since,omitempty"`
	// Until bounds CreatedAt above; nil means unbounded.
	Until *time.Time `json:"until,omitempty"`
	// Limit caps the MERGED result, not each provider's; 0 means the default.
	Limit int `json:"limit,omitempty"`
}

// ScoredEntry is one hit with the rank the provider or ranker gave it.
type ScoredEntry struct {
	// Entry is the stored entry, unmodified.
	Entry Entry `json:"entry"`
	// Score is comparable only within one Source; providers use different
	// scales, which is why merging goes through the ranker rather than a sort.
	Score float64 `json:"score"`
	// Source is the SearchProvider.Name that produced the hit.
	Source string `json:"source"`
}

// SearchResult is one provider's answer.
type SearchResult struct {
	// Entries are this provider's hits, its own best-first.
	Entries []ScoredEntry `json:"entries"`
	// DroppedFilters names request filters this provider could not honor, so a
	// narrower question silently answered as a wider one is visible.
	DroppedFilters []string `json:"droppedFilters,omitempty"`
}

// MergedSearchResult is what the composite searcher returns to a caller.
type MergedSearchResult struct {
	// Entries are the ranked, authorization-filtered hits across all providers.
	Entries []ScoredEntry `json:"entries"`
	// PerProvider keeps each provider's raw answer for diagnosis; a provider
	// that errored is absent rather than present-and-empty. Each answer may
	// hold one hit more than SearchRequest.Limit — the probe row the searcher
	// asks for to decide Truncated, kept here because this is the raw answer.
	PerProvider map[string]SearchResult `json:"perProvider,omitempty"`
	// Truncated reports that SearchRequest.Limit ended this page before the
	// matches did: at least one further hit ranked below the cut. False means
	// every match is in Entries.
	//
	// Like QueryResult.Truncated it describes the READ: an Authorizer
	// post-filter may leave fewer than Limit hits in Entries with Truncated
	// still true.
	Truncated bool `json:"truncated"`
}

// SearchCapabilities is a provider's declaration of what it can honor; the
// composite searcher never lifts a provider beyond what it claims here.
type SearchCapabilities struct {
	// TextSearch: honors SearchRequest.Text lexically.
	TextSearch bool `json:"textSearch"`
	// VectorSearch: honors SearchRequest.Text by embedding similarity.
	VectorSearch bool `json:"vectorSearch"`
	// FieldFilters: honors SearchRequest.Fields.
	FieldFilters bool `json:"fieldFilters"`
	// TagFilters: honors SearchRequest.Tags.
	TagFilters bool `json:"tagFilters"`
	// TimeRange: honors SearchRequest.Since / Until.
	TimeRange bool `json:"timeRange"`
	// LinkTraversal: can follow entry links while searching.
	LinkTraversal bool `json:"linkTraversal"`
}

// SearchProvider is one ranked-retrieval backend. The Local facade fans index
// updates out to every registered provider on Put/Delete/DeleteScope, logging
// and continuing past a failure rather than failing the write.
type SearchProvider interface {
	Name() string
	SearchCapabilities() SearchCapabilities
	Search(ctx context.Context, req SearchRequest) (SearchResult, error)
	Index(ctx context.Context, scope Scope, kind, id string, content []byte) error
	DeleteIndex(ctx context.Context, scope Scope, kind, id string) error
	DeleteScopeIndex(ctx context.Context, scope Scope) error
}

// EmbeddingProvider turns text into a vector for similarity search.
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Ranker merges per-provider results, whose scores are not comparable across
// providers, into one ordering.
type Ranker interface {
	Rank(results map[string]SearchResult, limit int) []ScoredEntry
}

// Searcher is the search executor; the Local facade optionally holds one, and
// Search returns ErrNoSearchProviders when it does not.
type Searcher interface {
	Search(ctx context.Context, req SearchRequest) (MergedSearchResult, error)
}
