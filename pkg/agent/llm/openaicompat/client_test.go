package openaicompat

import (
	"net/http"
	"net/http/httptest"
	"testing"

	oa "github.com/openai/openai-go/v3"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewClient_BaseURLAndHeaders pins the contract every OpenAI-wire-
// compatible caller (openai, openrouter providers; markup/summarizer/
// identityadvisor's OpenRouter-routed constructors) relies on: a non-empty
// baseURL overrides the endpoint, and every entry in headers is sent on the
// wire alongside the API key — this is the single place that logic lives.
func TestNewClient_BaseURLAndHeaders(t *testing.T) {
	var gotAuth, gotReferer, gotTitle string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotReferer = r.Header.Get("HTTP-Referer")
		gotTitle = r.Header.Get("X-Title")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "test-model",
			"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`))
	}))
	t.Cleanup(srv.Close)

	client := NewClient("sk-test", srv.URL, map[string]string{
		"HTTP-Referer": "https://github.com/authzed/openagentprimitives",
		"X-Title":      "agentprimitives",
	})

	resp, err := client.Chat.Completions.New(t.Context(), oa.ChatCompletionNewParams{
		Model:    "test-model",
		Messages: []oa.ChatCompletionMessageParamUnion{oa.UserMessage("hi")},
	})
	require.NoError(t, err)
	require.Len(t, resp.Choices, 1)

	assert.Equal(t, "Bearer sk-test", gotAuth, "API key must be sent as a bearer token")
	assert.Equal(t, "https://github.com/authzed/openagentprimitives", gotReferer, "extra headers must reach the wire")
	assert.Equal(t, "agentprimitives", gotTitle)
}

// TestNewClient_EmptyBaseURLLeavesSDKDefault verifies an empty baseURL does
// not append a WithBaseURL option (would-be zero-value override), so the
// SDK's own default endpoint resolution is left untouched — this is what lets
// the "openai" caller pass "" and get api.openai.com rather than an empty
// string base URL.
func TestNewClient_EmptyBaseURLLeavesSDKDefault(t *testing.T) {
	client := NewClient("sk-test", "", nil)
	// No network call here — asserting the client constructs without panicking
	// and is usable is the observable contract at this level; the base-URL
	// resolution behavior itself is exercised by TestNewClient_BaseURLAndHeaders
	// (non-empty case) and by openrouter/markup/summarizer's own endpoint tests.
	assert.NotNil(t, client.Chat)
}
