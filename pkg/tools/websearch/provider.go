// Package websearch provides a swappable, client-dispatched interface for
// web-search and web-fetch capabilities the gen agent uses to discover CLI
// tools and MCP servers.
//
// Client-dispatched only, deliberately. An LLM provider's own inline search
// (Anthropic's server-side web_search/web_fetch tools, e.g.) never yields a
// tool_use the agent loop dispatches — the result lands as a content block
// the model reads directly, so it never becomes a tool.Result. It therefore
// never passes through the runner's untrusted-content wrapper, never reaches
// a content-guard subject, and never touches the toolguard byte budget: every
// control this platform has over tool output is absent by construction.
//
// A Provider here always dispatches through the agent loop's normal
// tool.Tool path (ClientTools), so a real backend sees every one of those
// controls exactly like any other tool call.
package websearch

import (
	"context"
	"net/http"

	agenttool "github.com/authzed/openagentprimitives/pkg/agent/tool"
)

// Provider is the swappable backend for web-search and web-fetch.
type Provider interface {
	// Name returns a short identifier for logging and registry lookups;
	// "fake" and this package's registered real backends are examples.
	Name() string

	// ClientTools returns local agent.Tool implementations the agent loop
	// dispatches on tool_use.
	ClientTools() []agenttool.Tool
}

// Backend is the registrable descriptor for a Provider implementation.
// Implementations are stateless singletons registered at init() (see
// pkg/tools/websearch/registry); everything stateful — the HTTP client, the
// credential — lives on the Provider New returns. This mirrors
// sandboxkinds.Kind.NewRuntime and channelkinds.Kind.NewListener: which
// backend is selected is a registration-time decision, what it is
// constructed with is a call-time one.
type Backend interface {
	// Name is the registry key.
	Name() string

	// New constructs a live Provider from deps. Returns an error when the
	// backend cannot be constructed from what it was given (e.g. a missing
	// credential) — the same fail-closed shape sandboxkinds.Kind.NewRuntime
	// uses for an unavailable backend.
	New(deps Deps) (Provider, error)
}

// Deps is what a Backend needs to construct a live Provider.
type Deps struct {
	// HTTPClient is the caller's dial policy. Production MUST inject
	// safehttp.Client() so the SSRF guard actually applies to every request a
	// backend makes; tests inject an httptest server's client. A Backend must
	// never construct its own client — hardcoding one here would make the
	// SSRF guard unenforceable from outside this package.
	HTTPClient *http.Client

	// APIKey is the backend's credential. Resolving it (an AgentIdentity
	// secret, an env var, …) is the caller's concern, not this package's.
	APIKey string
}

// SearchResult is the kind-agnostic shape every provider maps its backend's
// response onto.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// FetchResult is the kind-agnostic shape for web_fetch.
type FetchResult struct {
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	// Body is the page text. The provider truncates to a sensible cap
	// (a few hundred KB) before returning so the agent doesn't blow
	// its context window on one page.
	Body string `json:"body"`
}

// SearchOpts is reserved for future per-call options (max results,
// domain restriction, etc.). Empty for now; client providers may
// ignore unknown fields.
type SearchOpts struct {
	MaxResults int
	// AllowedDomains restricts results to these domains. Provider-
	// best-effort; some backends ignore.
	AllowedDomains []string
}

// SearchExecutor is the optional interface a client-tool provider
// embeds in its agent.Tool to actually run the search. The agent
// loop never calls this directly — the Tool.Execute the provider
// returns from ClientTools() is what actually does the search;
// SearchExecutor is what the Tool.Execute uses internally. Exported
// so impls can share a default implementation if they want to.
type SearchExecutor interface {
	Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error)
	Fetch(ctx context.Context, url string) (*FetchResult, error)
}
