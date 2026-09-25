package markup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAIGenerate(t *testing.T) {
	const body = `{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-5.4-mini",
		"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"` + "```html" + `\n<div class=\"card\"><h1>Sample</h1></div>\n` + "```" + `"}}],
		"usage": {"prompt_tokens": 8, "completion_tokens": 12, "total_tokens": 20}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := NewOpenAI("sk-test")
	assert.Equal(t, "openai", p.Name())

	got, err := p.Generate(context.Background(), "a card with class 'card'")
	require.NoError(t, err)
	// Fences stripped, trimmed.
	assert.Equal(t, `<div class="card"><h1>Sample</h1></div>`, got)
}

func TestNewForProvider(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		wantName string
		wantType Provider
	}{
		{name: "openai routes to OpenAIProvider", provider: "openai", wantName: "openai", wantType: &OpenAIProvider{}},
		{name: "openrouter routes to OpenAIProvider (not the anthropic default)", provider: "openrouter", wantName: "openrouter", wantType: &OpenAIProvider{}},
		{name: "anthropic routes to AnthropicProvider", provider: "anthropic", wantName: "anthropic", wantType: &AnthropicProvider{}},
		{name: "empty default routes to AnthropicProvider", provider: "", wantName: "anthropic", wantType: &AnthropicProvider{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NewForProvider(tc.provider, "sk-test")
			require.NotNil(t, got)
			assert.Equal(t, tc.wantName, got.Name())
			assert.IsType(t, tc.wantType, got)
		})
	}
}

// TestNewOpenRouter_UsesOpenRouterEndpointAndModel exercises the openrouter
// branch end-to-end against a fake OpenRouter-shaped server, asserting the
// key routes to the OpenRouter base URL (not Anthropic's/OpenAI's default)
// and the request carries the OpenRouter-specific model id.
func TestNewOpenRouter_UsesOpenRouterEndpointAndModel(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		gotModel = body.Model

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "` + OpenRouterModel + `",
			"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"<div class=\"card\"></div>"}}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 4, "total_tokens": 12}
		}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	p := NewOpenRouter("sk-or-test")
	assert.Equal(t, "openrouter", p.Name())

	got, err := p.Generate(context.Background(), "a card")
	require.NoError(t, err)
	assert.Equal(t, `<div class="card"></div>`, got)
	assert.Equal(t, OpenRouterModel, gotModel, "request must use the OpenRouter model id, not the OpenAI one")
}
