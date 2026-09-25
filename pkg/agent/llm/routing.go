package llm

// OpenRouterRouting expresses OpenRouter's dynamic model-selection preferences.
// It is spec: a pure function of admin/AgentClass input, never an observation.
type OpenRouterRouting struct {
	Models         []string `json:"models,omitempty"`         // fallback list (tried in order)
	Sort           string   `json:"sort,omitempty"`           // "price" | "throughput" | "latency"
	Order          []string `json:"order,omitempty"`          // provider slugs, in order
	Only           []string `json:"only,omitempty"`           // allow only these provider slugs
	Ignore         []string `json:"ignore,omitempty"`         // skip these provider slugs
	AllowFallbacks *bool    `json:"allowFallbacks,omitempty"` // default true
	// RequireParameters: unset defaults to TRUE when the request carries tools
	// (the openrouter provider injects it — see
	// pkg/agent/llm/openrouter/extra.go's providerObject); explicit values are
	// honored verbatim. OpenRouter's own wire default is false — agents need
	// tools, so auto/fallback routing must only pick providers that support
	// the request's parameters.
	RequireParameters *bool               `json:"requireParameters,omitempty"`
	DataCollection    string              `json:"dataCollection,omitempty"` // "allow" | "deny"
	MaxPrice          *OpenRouterMaxPrice `json:"maxPrice,omitempty"`       // per-million-token price caps

	// Auto-router hints — apply when the model is "openrouter/auto".
	AllowedModels       []string `json:"allowedModels,omitempty"`       // narrow the auto-router candidate set
	CostQualityTradeoff *float64 `json:"costQualityTradeoff,omitempty"` // auto-router cost/quality tradeoff (OpenRouter docs example: 3; range not formally documented)
}

// OpenRouterMaxPrice caps per-model price, in USD per MILLION tokens
// (OpenRouter's max_price units; e.g. Prompt: 1 means at most $1/M prompt
// tokens). 0 on an axis means "no cap on this axis" (see
// pkg/platform/settings/routingmerge.go's tightenPrice and
// pkg/agent/llm/openrouter/extra.go's maxPriceObject), not "cap at $0".
type OpenRouterMaxPrice struct {
	Prompt     float64 `json:"prompt,omitempty"`     // max prompt price, USD per million tokens (0 = no cap)
	Completion float64 `json:"completion,omitempty"` // max completion price, USD per million tokens (0 = no cap)
}
