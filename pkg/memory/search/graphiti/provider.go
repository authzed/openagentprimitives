package graphiti

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

var kindPrefixes = map[string]string{
	"entity":    "ent-",
	"fact":      "fact-",
	"episode":   "ep-",
	"community": "comm-",
	"saga":      "saga-",
}

type GraphitiSearchProvider struct {
	endpoint string
	client   *http.Client
}

var _ memory.SearchProvider = (*GraphitiSearchProvider)(nil)

func New(endpoint string) *GraphitiSearchProvider {
	return &GraphitiSearchProvider{
		endpoint: endpoint,
		client:   &http.Client{Timeout: 30 * time.Second},
	}
}

func (*GraphitiSearchProvider) Name() string { return "graphiti" }

func (p *GraphitiSearchProvider) Endpoint() string     { return p.endpoint }
func (p *GraphitiSearchProvider) Client() *http.Client { return p.client }

func (*GraphitiSearchProvider) SearchCapabilities() memory.SearchCapabilities {
	return memory.SearchCapabilities{
		TextSearch:    true,
		VectorSearch:  true,
		LinkTraversal: true,
	}
}

// graphitiSearchRequest is the Graphiti server's /search request body.
type graphitiSearchRequest struct {
	// Query is the natural-language search text.
	Query string `json:"query"`
	// MaxFacts caps the facts returned; omitted lets Graphiti pick.
	MaxFacts int `json:"max_facts,omitempty"`
	// GroupIDs restricts the search to these session graphs. OMITTING IT
	// searches every group, so it must be set for any scoped read.
	GroupIDs []string `json:"group_ids,omitempty"`
}

// GraphitiSearchResponse matches the official Graphiti server's /search response.
type GraphitiSearchResponse struct {
	// Facts are the hits, Graphiti's own best-first order.
	Facts []GraphitiResult `json:"facts"`
}

// GraphitiResult represents an entity or fact returned by Graphiti.
type GraphitiResult struct {
	// Type distinguishes an entity result from a fact result.
	Type string `json:"type"`
	// UUID is Graphiti's identifier, stable across re-ingestion.
	UUID string `json:"uuid"`
	// Name is the entity name or relationship type.
	Name string `json:"name"`
	// Summary is Graphiti's rolling description; set on entities.
	Summary string `json:"summary"`
	// Fact is the natural-language assertion; set on facts.
	Fact string `json:"fact"`
	// Score is Graphiti's own relevance, higher-is-better, and NOT comparable
	// with any other provider's — RRF is what makes the merge meaningful.
	Score float64 `json:"score"`
}

func (p *GraphitiSearchProvider) Search(ctx context.Context, req memory.SearchRequest) (memory.SearchResult, error) {
	if req.Text == "" {
		return memory.SearchResult{
			DroppedFilters: []string{"text (required for graphiti)"},
		}, nil
	}

	var groupIDs []string
	for _, sc := range req.Scopes {
		if sc.ID != "" {
			groupIDs = append(groupIDs, sc.ID)
		}
	}

	body, err := json.Marshal(graphitiSearchRequest{Query: req.Text, MaxFacts: req.Limit, GroupIDs: groupIDs})
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("graphiti: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/search", bytes.NewReader(body))
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("graphiti: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return memory.SearchResult{}, fmt.Errorf("graphiti: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return memory.SearchResult{}, fmt.Errorf("graphiti: status %d: %s", resp.StatusCode, msg)
	}

	var gResp GraphitiSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&gResp); err != nil {
		return memory.SearchResult{}, fmt.Errorf("graphiti: decode response: %w", err)
	}

	scope := memory.Scope{}
	if len(req.Scopes) > 0 {
		scope = req.Scopes[0]
	}

	entries := make([]memory.ScoredEntry, 0, len(gResp.Facts))
	for i, r := range gResp.Facts {
		prefix := kindPrefixes[r.Type]
		if prefix == "" {
			prefix = "fact-"
		}
		summary := r.Fact
		if summary == "" {
			summary = r.Summary
		}
		content, _ := json.Marshal(map[string]string{"name": r.Name, "fact": summary})
		entries = append(entries, memory.ScoredEntry{
			Entry: memory.Entry{
				Scope: scope, Kind: "fact", ID: prefix + r.UUID, Content: content,
			},
			Score: 1.0 - float64(i)*0.01, Source: "graphiti",
		})
	}

	var dropped []string
	if len(req.Kinds) > 0 {
		dropped = append(dropped, "kinds")
	}
	if len(req.Tags) > 0 {
		dropped = append(dropped, "tags")
	}
	if len(req.Fields) > 0 {
		dropped = append(dropped, "fields")
	}
	if req.Since != nil || req.Until != nil {
		dropped = append(dropped, "timeRange")
	}

	return memory.SearchResult{Entries: entries, DroppedFilters: dropped}, nil
}

// Index, DeleteIndex and DeleteScopeIndex are DELIBERATE no-ops. They keep the
// graphiti backend a plain memory.SearchProvider the facade drives like every
// other, rather than a special case it must branch around.
//
// Graphiti owns its own corpus: entries reach the graph through episode
// ingestion (kg_ingestion's ScopeHooks calling KGProvider.Ingest), where entity
// extraction, dedup and contradiction detection run asynchronously. There is no
// per-entry index to mirror an Entry into, so writing one would invent state the
// graph does not have.
//
// CONSEQUENCE: a memory entry deleted from the backend is NOT retracted from the
// graph — facts already extracted from it survive. Retraction is a Graphiti-side
// operation, not an index delete.
func (*GraphitiSearchProvider) Index(_ context.Context, _ memory.Scope, _, _ string, _ []byte) error {
	return nil
}
func (*GraphitiSearchProvider) DeleteIndex(_ context.Context, _ memory.Scope, _, _ string) error {
	return nil
}
func (*GraphitiSearchProvider) DeleteScopeIndex(_ context.Context, _ memory.Scope) error {
	return nil
}
