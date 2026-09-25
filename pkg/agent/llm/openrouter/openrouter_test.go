package openrouter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/agent/llm"
)

// sseModelAndCost writes a minimal OpenRouter chat-completion SSE stream whose
// final chunk reports a served model different from the request + a usage.cost.
func sseModelAndCost(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fl, _ := w.(http.Flusher)
	chunks := []string{
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"anthropic/claude-3.5-sonnet","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"x","object":"chat.completion.chunk","created":1,"model":"anthropic/claude-3.5-sonnet","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"cost":0.0004}}`,
	}
	for _, c := range chunks {
		_, _ = w.Write([]byte("data: " + c + "\n\n"))
		if fl != nil {
			fl.Flush()
		}
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

// TestEffectiveBaseURL pins the base-URL resolution every OpenRouter-routed
// client (this package's Provider plus the runner's secondary-LLM helpers)
// relies on: OPENROUTER_BASE_URL overrides when set, DefaultBaseURL otherwise.
func TestEffectiveBaseURL(t *testing.T) {
	t.Run("no override ⇒ DefaultBaseURL", func(t *testing.T) {
		assert.Equal(t, DefaultBaseURL, EffectiveBaseURL())
	})
	t.Run("OPENROUTER_BASE_URL set ⇒ override wins", func(t *testing.T) {
		t.Setenv(envBaseURL, "https://proxy.example/v1")
		assert.Equal(t, "https://proxy.example/v1", EffectiveBaseURL())
	})
}

// TestAttributionHeaders pins the header set every OpenRouter-routed client
// sends for attribution on the openrouter.ai leaderboards/logs.
func TestAttributionHeaders(t *testing.T) {
	h := AttributionHeaders()
	assert.Equal(t, "https://github.com/authzed/openagentprimitives", h["HTTP-Referer"])
	assert.Equal(t, "agentprimitives", h["X-Title"])
}

func TestSend_CapturesServedModelAndReportedCost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sseModelAndCost(w)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	p := New("test-key")
	resp, err := p.Send(context.Background(), llm.Request{
		Model:     "openrouter/auto",
		Messages:  []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}},
		MaxTokens: 64,
	})
	require.NoError(t, err)
	assert.Equal(t, "anthropic/claude-3.5-sonnet", resp.Model)
	require.NotNil(t, resp.Usage.CostUSD)
	assert.InDelta(t, 0.0004, *resp.Usage.CostUSD, 1e-9)
}

// TestSend_ForwardsRoutingSubconfig asserts the outgoing request body carries
// the OpenRouter routing subconfig (models[]/provider{}) + usage.include, so a
// server that inspects the body (rather than just replying with a fixed
// stream) sees the caller's ProviderRouting reflected.
func TestSend_ForwardsRoutingSubconfig(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		sseModelAndCost(w)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	p := New("test-key")
	allowFallbacks := true
	_, err := p.Send(context.Background(), llm.Request{
		Model:    "openrouter/auto",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}},
		ProviderRouting: &llm.OpenRouterRouting{
			Models:         []string{"anthropic/claude-3.5-sonnet", "openai/gpt-5.4"},
			Sort:           "price",
			AllowFallbacks: &allowFallbacks,
		},
	})
	require.NoError(t, err)

	require.NotNil(t, gotBody["models"])
	require.NotNil(t, gotBody["provider"])
	usage, ok := gotBody["usage"].(map[string]any)
	require.True(t, ok, "usage field must be an object")
	assert.Equal(t, true, usage["include"])
}

// TestSend_ToolsPresentDefaultsRequireParameters is the end-to-end FIX B pin:
// a request with Tools set and no explicit RequireParameters must reach the
// wire with provider.require_parameters:true, so OpenRouter's auto/fallback
// routing only picks providers that can honor the request's tool calls.
func TestSend_ToolsPresentDefaultsRequireParameters(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		sseModelAndCost(w)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENROUTER_BASE_URL", srv.URL)

	p := New("test-key")
	_, err := p.Send(context.Background(), llm.Request{
		Model:    "openrouter/auto",
		Messages: []llm.Message{{Role: "user", Content: []llm.ContentBlock{{Type: "text", Text: "hello"}}}},
		Tools:    []llm.ToolDef{{Name: "get_weather", Description: "look up weather"}},
	})
	require.NoError(t, err)

	prov, ok := gotBody["provider"].(map[string]any)
	require.True(t, ok, "provider object must be created to carry the tools-default even with no ProviderRouting set")
	assert.Equal(t, true, prov["require_parameters"])
}
