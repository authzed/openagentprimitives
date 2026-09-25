package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/runner/identityadvisor"
)

// TestBuildIdentityRecommender pins the provider→backend dispatch for the
// identityMode=dynamic advisory LLM. Before this fix, every session (openai
// and openrouter included) got identityadvisor.NewAnthropic(apiKey) regardless
// of the session's actual provider; an OpenAI/OpenRouter-provisioned key sent
// to Anthropic's API fails outright. Each case asserts both the concrete
// backend type (not the anthropic default) and the Name() a log line would
// show.
func TestBuildIdentityRecommender(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		wantName string
		wantType identityadvisor.Provider
	}{
		{name: "anthropic routes to AnthropicProvider", provider: "anthropic", wantName: "anthropic", wantType: &identityadvisor.AnthropicProvider{}},
		{name: "empty default routes to AnthropicProvider", provider: "", wantName: "anthropic", wantType: &identityadvisor.AnthropicProvider{}},
		{name: "openai routes to OpenAICompatibleProvider (not anthropic)", provider: "openai", wantName: "openai", wantType: &identityadvisor.OpenAICompatibleProvider{}},
		{name: "openrouter routes to OpenAICompatibleProvider (not anthropic)", provider: "openrouter", wantName: "openrouter", wantType: &identityadvisor.OpenAICompatibleProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildIdentityRecommender(tc.provider, "sk-test")
			require.NotNil(t, got)
			assert.Equal(t, tc.wantName, got.Name())
			assert.IsType(t, tc.wantType, got)
		})
	}
}
