package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

const WebSearchName = "web_search"
const WebSearchSchema = `{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "query": { "type": "string", "description": "search query (≤ 200 chars)" }
  },
  "required": ["query"]
}`

// WebSearchClient is the seam; tests inject a stub. Production wires
// pkg/tools/websearch, which has no standalone Search function — it exposes the
// SearchExecutor interface, and this signature matches its Search method exactly.
var WebSearchClient func(ctx context.Context, query string, opts websearch.SearchOpts) ([]websearch.SearchResult, error)

func WebSearchRun(ctx context.Context, raw json.RawMessage) (string, error) {
	var args struct {
		// The search query; refused when empty or longer than 200 characters.
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if args.Query == "" {
		return "", fmt.Errorf("web_search: query required")
	}
	if len(args.Query) > 200 {
		return "", fmt.Errorf("web_search: query too long")
	}
	if WebSearchClient == nil {
		return "", fmt.Errorf("web_search: no client configured")
	}
	results, err := WebSearchClient(ctx, args.Query, websearch.SearchOpts{MaxResults: 5})
	if err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	out, _ := json.Marshal(results)
	return string(out), nil
}
