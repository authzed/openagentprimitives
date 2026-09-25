package openrouter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

func TestExtraBodyFields(t *testing.T) {
	allowFallbacks := false
	requireParams := true
	tradeoff := 0.3

	cases := []struct {
		name     string
		routing  *llm.OpenRouterRouting
		hasTools bool
		check    func(t *testing.T, fields map[string]any)
	}{
		{
			name:    "nil routing: only usage.include",
			routing: nil,
			check: func(t *testing.T, fields map[string]any) {
				assert.Len(t, fields, 1, "nil routing must inject nothing beyond usage.include")
				usage, ok := fields["usage"].(map[string]any)
				require.True(t, ok, "usage must be an object")
				assert.Equal(t, true, usage["include"])
			},
		},
		{
			name:    "empty routing: only usage.include",
			routing: &llm.OpenRouterRouting{},
			check: func(t *testing.T, fields map[string]any) {
				assert.Len(t, fields, 1, "a routing struct with no fields set must inject nothing beyond usage.include")
			},
		},
		{
			name: "full routing: models + provider populated with the mapped keys",
			routing: &llm.OpenRouterRouting{
				Models:            []string{"anthropic/claude-3.5-sonnet", "openai/gpt-5.4"},
				Sort:              "price",
				Order:             []string{"anthropic", "openai"},
				Only:              []string{"anthropic"},
				Ignore:            []string{"azure"},
				AllowFallbacks:    &allowFallbacks,
				RequireParameters: &requireParams,
				DataCollection:    "deny",
				MaxPrice:          &llm.OpenRouterMaxPrice{Prompt: 1.5, Completion: 3},
			},
			check: func(t *testing.T, fields map[string]any) {
				assert.Equal(t, []string{"anthropic/claude-3.5-sonnet", "openai/gpt-5.4"}, fields["models"])
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok, "provider must be an object")
				assert.Equal(t, "price", prov["sort"])
				assert.Equal(t, []string{"anthropic", "openai"}, prov["order"])
				assert.Equal(t, []string{"anthropic"}, prov["only"])
				assert.Equal(t, []string{"azure"}, prov["ignore"])
				assert.Equal(t, false, prov["allow_fallbacks"])
				assert.Equal(t, true, prov["require_parameters"])
				assert.Equal(t, "deny", prov["data_collection"])
				maxPrice, ok := prov["max_price"].(map[string]any)
				require.True(t, ok, "max_price must be an object")
				assert.Equal(t, 1.5, maxPrice["prompt"])
				assert.Equal(t, 3.0, maxPrice["completion"])
				assert.Nil(t, fields["plugins"], "no auto-router hints were set")
			},
		},
		{
			name: "auto-router hints: plugins populated",
			routing: &llm.OpenRouterRouting{
				AllowedModels:       []string{"anthropic/claude-3.5-sonnet"},
				CostQualityTradeoff: &tradeoff,
			},
			check: func(t *testing.T, fields map[string]any) {
				assert.Nil(t, fields["models"])
				assert.Nil(t, fields["provider"])
				plugins, ok := fields["plugins"].([]any)
				require.True(t, ok, "plugins must be a list")
				require.Len(t, plugins, 1)
				plugin, ok := plugins[0].(map[string]any)
				require.True(t, ok, "plugin entry must be an object")
				assert.Equal(t, "auto-router", plugin["id"])
				// allowed_models/cost_quality_tradeoff are SIBLINGS of "id" on the
				// wire (verified against openrouter.ai/docs/features/model-routing) —
				// there is no "config" nesting.
				assert.Equal(t, []string{"anthropic/claude-3.5-sonnet"}, plugin["allowed_models"])
				assert.Equal(t, 0.3, plugin["cost_quality_tradeoff"])
				assert.NotContains(t, plugin, "config", "fields must be siblings of id, not nested under config")
			},
		},
		// --- RequireParameters tools-default (FIX B) ---
		{
			name:     "tools + unset RequireParameters: require_parameters defaults true",
			routing:  nil,
			hasTools: true,
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok, "provider must be created even with nil routing, to carry the tools-default")
				assert.Equal(t, true, prov["require_parameters"])
			},
		},
		{
			name:     "tools + empty routing (RequireParameters unset): defaults true",
			routing:  &llm.OpenRouterRouting{},
			hasTools: true,
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, true, prov["require_parameters"])
			},
		},
		{
			name:     "tools + explicit RequireParameters=false: explicit value honored, not overridden",
			routing:  &llm.OpenRouterRouting{RequireParameters: boolPtr(false)},
			hasTools: true,
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, false, prov["require_parameters"])
			},
		},
		{
			name:     "tools + explicit RequireParameters=true: explicit value honored",
			routing:  &llm.OpenRouterRouting{RequireParameters: boolPtr(true)},
			hasTools: true,
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, true, prov["require_parameters"])
			},
		},
		{
			name:     "no tools + unset RequireParameters: key absent (today's behavior)",
			routing:  nil,
			hasTools: false,
			check: func(t *testing.T, fields map[string]any) {
				assert.Nil(t, fields["provider"], "no tools + no routing must inject nothing beyond usage.include")
			},
		},
		{
			name:     "no tools + explicit RequireParameters=true: explicit always wins",
			routing:  &llm.OpenRouterRouting{RequireParameters: boolPtr(true)},
			hasTools: false,
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, true, prov["require_parameters"])
			},
		},
		// --- max_price units + zero-axis omission (FIX C) ---
		{
			name:    "max_price: half-set cap emits only that axis",
			routing: &llm.OpenRouterRouting{MaxPrice: &llm.OpenRouterMaxPrice{Prompt: 1.5}},
			check: func(t *testing.T, fields map[string]any) {
				prov, ok := fields["provider"].(map[string]any)
				require.True(t, ok)
				maxPrice, ok := prov["max_price"].(map[string]any)
				require.True(t, ok, "max_price must be an object")
				assert.Equal(t, 1.5, maxPrice["prompt"])
				assert.NotContains(t, maxPrice, "completion", "the unset (zero) axis must be omitted, not sent as a $0 cap")
			},
		},
		{
			name:    "max_price: both axes zero omits max_price entirely",
			routing: &llm.OpenRouterRouting{MaxPrice: &llm.OpenRouterMaxPrice{}},
			check: func(t *testing.T, fields map[string]any) {
				assert.Nil(t, fields["provider"], "a MaxPrice struct with both axes zero must inject nothing")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.check(t, extraBodyFields(tc.routing, tc.hasTools))
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRoutingOptions_AlwaysIncludesUsageOption(t *testing.T) {
	// routingOptions must always yield at least the usage.include option,
	// regardless of routing — Send relies on this to read back the reported
	// cost (see reportedCostUSD).
	assert.Len(t, routingOptions(nil, false), 1)

	withModels := routingOptions(&llm.OpenRouterRouting{Models: []string{"anthropic/claude-3.5-sonnet"}}, false)
	assert.Len(t, withModels, 2, "usage.include + models")
}

// TestRoutingOptions_HasToolsInjectsProviderEvenWithNilRouting proves the
// require_parameters tools-default flows all the way through routingOptions
// (not just extraBodyFields) when there is no routing configured at all.
func TestRoutingOptions_HasToolsInjectsProviderEvenWithNilRouting(t *testing.T) {
	opts := routingOptions(nil, true)
	assert.Len(t, opts, 2, "usage.include + provider (require_parameters default)")
}

func TestReportedCostUSD(t *testing.T) {
	cases := []struct {
		name      string
		rawJSON   string
		wantCost  float64
		wantFound bool
	}{
		{
			name:      "cost present",
			rawJSON:   `{"prompt_tokens":5,"completion_tokens":1,"cost":0.0004}`,
			wantCost:  0.0004,
			wantFound: true,
		},
		{
			name:      "cost absent",
			rawJSON:   `{"prompt_tokens":5,"completion_tokens":1}`,
			wantFound: false,
		},
		{
			name:      "empty string (no usage-bearing chunk observed)",
			rawJSON:   "",
			wantFound: false,
		},
		{
			name:      "malformed JSON",
			rawJSON:   "{not json",
			wantFound: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cost, ok := reportedCostUSD(tc.rawJSON)
			assert.Equal(t, tc.wantFound, ok)
			if tc.wantFound {
				assert.InDelta(t, tc.wantCost, cost, 1e-9)
			}
		})
	}
}
