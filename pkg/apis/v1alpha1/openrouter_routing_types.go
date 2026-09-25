package v1alpha1

// OpenRouterRouting configures OpenRouter's dynamic model-selection behavior
// (auto-router, fallback list, provider preferences). It is spec — a pure
// function of admin/AgentClass input — and is only consulted when the owning
// ModelCatalogEntry's Provider == "openrouter"; on any other provider it is
// ignored. This is the CRD-facing (kubebuilder-validated) mirror of the
// neutral `llm.OpenRouterRouting` type; `pkg/platform/settings/resolve.go` converts
// between the two (a small `toLLM()` converter) — keep the two in sync.
type OpenRouterRouting struct {
	// +optional
	// +listType=atomic
	Models []string `json:"models,omitempty"` // fallback list (tried in order)
	// +kubebuilder:validation:Enum=price;throughput;latency
	// +optional
	Sort string `json:"sort,omitempty"` // "price" | "throughput" | "latency"
	// +optional
	// +listType=atomic
	Order []string `json:"order,omitempty"` // provider slugs, in order
	// +optional
	// +listType=atomic
	Only []string `json:"only,omitempty"` // allow only these provider slugs
	// +optional
	// +listType=atomic
	Ignore []string `json:"ignore,omitempty"` // skip these provider slugs
	// +optional
	AllowFallbacks *bool `json:"allowFallbacks,omitempty"` // default true
	// RequireParameters: unset defaults to TRUE when the request carries tools
	// (the openrouter provider injects it — see
	// pkg/agent/llm/openrouter/extra.go's providerObject); explicit values are
	// honored verbatim. OpenRouter's own wire default is false — agents need
	// tools, so auto/fallback routing must only pick providers that support
	// the request's parameters.
	// +optional
	RequireParameters *bool `json:"requireParameters,omitempty"`
	// +kubebuilder:validation:Enum=allow;deny
	// +optional
	DataCollection string `json:"dataCollection,omitempty"` // "allow" | "deny"
	// +optional
	MaxPrice *OpenRouterMaxPrice `json:"maxPrice,omitempty"` // per-million-token price caps

	// Auto-router hints — apply when the model is "openrouter/auto".
	// +optional
	// +listType=atomic
	AllowedModels []string `json:"allowedModels,omitempty"` // narrow the auto-router candidate set
	// CostQualityTradeoff is intentionally unvalidated (no Minimum/Maximum
	// marker): OpenRouter documents no formal range for this value — its docs
	// example value is 3 — so a range marker would reject valid input.
	// +optional
	CostQualityTradeoff *float64 `json:"costQualityTradeoff,omitempty"` // auto-router cost/quality tradeoff (OpenRouter docs example: 3; range not formally documented)
}

// OpenRouterMaxPrice caps per-model price, in USD per MILLION tokens
// (OpenRouter's max_price units; e.g. Prompt: 1 means at most $1/M prompt
// tokens). 0 on an axis means "no cap on this axis" (see
// pkg/platform/settings/routingmerge.go's tightenPrice and
// pkg/agent/llm/openrouter/extra.go's maxPriceObject), not "cap at $0". Only
// consulted when the owning ModelCatalogEntry's Provider == "openrouter".
type OpenRouterMaxPrice struct {
	// +kubebuilder:validation:Minimum=0
	// +optional
	Prompt float64 `json:"prompt,omitempty"` // max prompt price, USD per million tokens (0 = no cap)
	// +kubebuilder:validation:Minimum=0
	// +optional
	Completion float64 `json:"completion,omitempty"` // max completion price, USD per million tokens (0 = no cap)
}
