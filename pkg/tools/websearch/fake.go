package websearch

import (
	"context"
	"encoding/json"
	"fmt"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
	"github.com/authzed/openagentprimitives/pkg/authz"
)

// FakeProvider is a client-tool provider returning programmable
// search/fetch results. Used by every later phase's tests to exercise
// the agent loop's web_search / web_fetch dispatch path without
// touching the network.
//
// Set SearchFn / FetchFn before exposing the tools to a runner; nil
// functions cause Tool.Execute to return a clear IsError result rather
// than panic.
type FakeProvider struct {
	SearchFn func(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)
	FetchFn  func(ctx context.Context, url string) (*FetchResult, error)
}

// NewFakeProvider returns an unconfigured FakeProvider. Set SearchFn
// and FetchFn before calling ClientTools().Execute.
func NewFakeProvider() *FakeProvider { return &FakeProvider{} }

func (*FakeProvider) Name() string { return "fake" }

func (p *FakeProvider) ClientTools() []agenttool.Tool {
	return []agenttool.Tool{
		&fakeSearchTool{p: p},
		&fakeFetchTool{p: p},
	}
}

// Compile-time interface check.
var _ Provider = (*FakeProvider)(nil)

// fakeSearchTool implements agenttool.Tool with Kind=KindMeta. The gen
// agent treats web_search as in-process (no sandbox, no MCP transport),
// and KindMeta is exactly that: a tool the runner dispatches itself.
// A dedicated "websearch" Kind would be premature — meta covers it.
type fakeSearchTool struct{ p *FakeProvider }

func (*fakeSearchTool) Name() string         { return "web_search" }
func (*fakeSearchTool) Kind() agenttool.Kind { return agenttool.KindMeta }
func (*fakeSearchTool) Description() string {
	return "Search the web for results matching the query."
}
func (*fakeSearchTool) Permission() authz.Permission {
	// web_search fetches from an external provider but does not map to a
	// SpiceDB-managed resource; treated as Passthrough until slice 2+.
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*fakeSearchTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*fakeSearchTool) InputSchema() json.RawMessage {
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

func (t *fakeSearchTool) Execute(ctx context.Context, args json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var in struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results,omitempty"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agenttool.Result{Content: "fake_search: invalid args: " + err.Error(), IsError: true}, nil
	}
	if t.p.SearchFn == nil {
		return agenttool.Result{Content: "fake_search: no SearchFn configured", IsError: true}, nil
	}
	results, err := t.p.SearchFn(ctx, in.Query, SearchOpts{MaxResults: in.MaxResults})
	if err != nil {
		return agenttool.Result{Content: "fake_search: " + err.Error(), IsError: true}, nil
	}
	out, _ := json.Marshal(results)
	return agenttool.Result{Content: fmt.Sprintf("results: %s", out)}, nil
}

// fakeFetchTool — same shape as fakeSearchTool but for web_fetch:
// arguments {url string}; returns FetchResult JSON-marshaled in Content.
type fakeFetchTool struct{ p *FakeProvider }

func (*fakeFetchTool) Name() string         { return "web_fetch" }
func (*fakeFetchTool) Kind() agenttool.Kind { return agenttool.KindMeta }
func (*fakeFetchTool) Description() string {
	return "Fetch the contents of a URL."
}
func (*fakeFetchTool) Permission() authz.Permission {
	// web_fetch retrieves arbitrary external URLs; no SpiceDB resource
	// mapping exists yet — treated as Passthrough until slice 2+.
	return authz.Permission{StateImpact: authz.Passthrough}
}
func (*fakeFetchTool) PermissionVariants() []authz.PermissionVariant { return nil }

func (*fakeFetchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"additionalProperties": false,
		"properties": {
			"url": {"type": "string", "description": "URL to fetch"}
		},
		"required": ["url"]
	}`)
}

func (t *fakeFetchTool) Execute(ctx context.Context, args json.RawMessage, _ *agenttool.SessionContext) (agenttool.Result, error) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return agenttool.Result{Content: "fake_fetch: invalid args: " + err.Error(), IsError: true}, nil
	}
	if t.p.FetchFn == nil {
		return agenttool.Result{Content: "fake_fetch: no FetchFn configured", IsError: true}, nil
	}
	result, err := t.p.FetchFn(ctx, in.URL)
	if err != nil {
		return agenttool.Result{Content: "fake_fetch: " + err.Error(), IsError: true}, nil
	}
	if result == nil {
		return agenttool.Result{Content: "fake_fetch: nil result", IsError: true}, nil
	}
	out, _ := json.Marshal(result)
	return agenttool.Result{Content: fmt.Sprintf("result: %s", out)}, nil
}
