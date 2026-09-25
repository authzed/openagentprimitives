package bravesearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

const (
	// envBaseURL overrides DefaultBaseURL for tests and any future proxy
	// deployment — the same seam pkg/agent/llm/openrouter uses for its own
	// upstream endpoint.
	envBaseURL = "BRAVE_SEARCH_BASE_URL"

	// DefaultBaseURL is the Brave Search API's web-search endpoint.
	DefaultBaseURL = "https://api.search.brave.com/res/v1/web/search"

	// maxFetchBytes caps how much of a fetched page's body this provider
	// returns, so one page can't blow the agent's context window. See
	// websearch.FetchResult.Body's doc comment.
	maxFetchBytes = 300 * 1024

	// maxSearchResults is the Brave Search API's own documented cap on
	// "count" for a single web-search request.
	maxSearchResults = 20
)

// EffectiveBaseURL returns BRAVE_SEARCH_BASE_URL when set (test/proxy
// override), else DefaultBaseURL.
func EffectiveBaseURL() string {
	if v := os.Getenv(envBaseURL); v != "" {
		return v
	}
	return DefaultBaseURL
}

// Provider is the Brave-Search-backed websearch.Provider. Constructed only
// via Backend.New — it never builds its own *http.Client, so the caller's
// dial policy (safehttp.Client() in production, an httptest client in
// tests) is what actually reaches the network.
type Provider struct {
	httpClient *http.Client
	apiKey     string
}

func (*Provider) Name() string { return KindName }

func (p *Provider) ClientTools() []agenttool.Tool {
	return []agenttool.Tool{
		&searchTool{p: p},
		&fetchTool{p: p},
	}
}

// braveSearchResponse is the subset of the Brave Search API's web-search
// response shape this provider reads.
type braveSearchResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
	} `json:"web"`
}

// Search implements websearch.SearchExecutor.
func (p *Provider) Search(ctx context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error) {
	u, err := url.Parse(EffectiveBaseURL())
	if err != nil {
		return nil, fmt.Errorf("bravesearch: invalid base URL: %w", err)
	}
	count := opts.MaxResults
	if count <= 0 || count > maxSearchResults {
		count = maxSearchResults
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	u.RawQuery = q.Encode()
	// AllowedDomains is provider-best-effort (see websearch.SearchOpts); the
	// Brave Search API has no per-request domain filter, so this backend
	// does not implement it.

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("bravesearch: build search request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", p.apiKey)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bravesearch: search request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("bravesearch: search returned %s: %s", resp.Status, body)
	}

	var parsed braveSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("bravesearch: decode search response: %w", err)
	}

	results := make([]websearch.SearchResult, 0, len(parsed.Web.Results))
	for _, r := range parsed.Web.Results {
		results = append(results, websearch.SearchResult{
			Title:   r.Title,
			URL:     r.URL,
			Snippet: r.Description,
		})
	}
	return results, nil
}

// Fetch implements websearch.SearchExecutor. It is a direct GET of the
// caller-supplied URL through the injected *http.Client — production wires
// safehttp.Client(), so the SSRF guard applies to this request exactly as it
// does to every other externally-reachable request in this codebase. Fetch
// does not reduce HTML to text, enforce a content-type allowlist, or spill
// to an artifact past its cap; that policy belongs to the sidecar tool this
// provider will sit behind, not to the provider.
func (p *Provider) Fetch(ctx context.Context, rawURL string) (*websearch.FetchResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bravesearch: build fetch request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bravesearch: fetch request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bravesearch: fetch returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return nil, fmt.Errorf("bravesearch: read fetch body: %w", err)
	}

	return &websearch.FetchResult{
		URL:         rawURL,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        string(body),
	}, nil
}

var (
	_ websearch.Provider       = (*Provider)(nil)
	_ websearch.SearchExecutor = (*Provider)(nil)
)
