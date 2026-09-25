package bravesearch

import (
	"context"
	"encoding/json"
	"fmt"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
	"github.com/authzed/openagentprimitives/pkg/tools/websearch"
)

// searchTool and fetchTool use the same tool names, schemas and KindMeta
// dispatch as pkg/tools/websearch's FakeProvider, so swapping the backend
// never changes the tool contract the agent (or a bundle fixture) sees.

type searchTool struct{ p *Provider }

func (*searchTool) Name() string         { return "web_search" }
func (*searchTool) Kind() agenttool.Kind { return agenttool.KindMeta }
func (*searchTool) Description() string {
	return "Search the web for results matching the query."
}
func (*searchTool) Permission() authz.Permission {
	// web_search fetches from an external provider but does not map to a
	// SpiceDB-managed resource; treated as Passthrough until slice 2+.
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*searchTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*searchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"query":       {"type": "string", "description": "search query"},
			"max_results": {"type": "integer", "description": "optional cap on results"}
		},
		"required": ["query"]
	}`)
}

func (t *searchTool) Execute(ctx context.Context, args json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var in struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agenttool.Result{Content: "web_search: invalid args: " + err.Error(), IsError: true}, nil
	}
	results, err := t.p.Search(ctx, in.Query, websearch.SearchOpts{MaxResults: in.MaxResults})
	if err != nil {
		return agenttool.Result{Content: "web_search: " + err.Error(), IsError: true}, nil
	}
	out, _ := json.Marshal(results)
	return agenttool.Result{Content: fmt.Sprintf("results: %s", out)}, nil
}

type fetchTool struct{ p *Provider }

func (*fetchTool) Name() string         { return "web_fetch" }
func (*fetchTool) Kind() agenttool.Kind { return agenttool.KindMeta }
func (*fetchTool) Description() string {
	return "Fetch the contents of a URL."
}
func (*fetchTool) Permission() authz.Permission {
	// web_fetch retrieves arbitrary external URLs; no SpiceDB resource
	// mapping exists yet — treated as Passthrough until slice 2+.
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*fetchTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*fetchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`)
}

func (t *fetchTool) Execute(ctx context.Context, args json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agenttool.Result{Content: "web_fetch: invalid args: " + err.Error(), IsError: true}, nil
	}
	result, err := t.p.Fetch(ctx, in.URL)
	if err != nil {
		return agenttool.Result{Content: "web_fetch: " + err.Error(), IsError: true}, nil
	}
	out, _ := json.Marshal(result)
	return agenttool.Result{Content: fmt.Sprintf("result: %s", out)}, nil
}
