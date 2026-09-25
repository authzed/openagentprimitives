package summarizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenAISummarize(t *testing.T) {
	// Canned Chat Completions response whose message content is the strict
	// {"summary": "..."} JSON the summarizer expects.
	const body = `{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-5.4-mini",
		"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\": \"Search contacts for Jane\"}"}}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 6, "total_tokens": 16}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL)

	p := NewOpenAI("sk-test")
	assert.Equal(t, "openai", p.Name())

	got, err := p.Summarize(context.Background(), Request{
		Tool: "search_contacts", ArgsJSON: `{"q":"Jane"}`,
		ResourceType: "contacts", ResourceID: "root", Permission: "search",
	})
	require.NoError(t, err)
	assert.Equal(t, "Search contacts for Jane", got)
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
			"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"summary\": \"Search contacts for Jane\"}"}}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 6, "total_tokens": 16}
		}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	p := NewOpenRouter("sk-or-test")
	assert.Equal(t, "openrouter", p.Name())

	got, err := p.Summarize(context.Background(), Request{
		Tool: "search_contacts", ArgsJSON: `{"q":"Jane"}`,
		ResourceType: "contacts", ResourceID: "root", Permission: "search",
	})
	require.NoError(t, err)
	assert.Equal(t, "Search contacts for Jane", got)
	assert.Equal(t, OpenRouterModel, gotModel, "request must use the OpenRouter model id, not the OpenAI one")
}
