package identityadvisor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	oa "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// TestOpenAICompatibleProvider_Recommend exercises Recommend end-to-end
// against a fake Chat-Completions server (mirrors
// pkg/agent/preview/markup's and pkg/agent/runner/approval/summarizer's
// openai_test.go pattern — a canned non-streaming JSON completion, since
// Recommend is a single round trip with no streaming), asserting a
// recommendation is produced and parsed from the canned model output.
func TestOpenAICompatibleProvider_Recommend(t *testing.T) {
	body := `{
		"id": "chatcmpl-1", "object": "chat.completion", "model": "` + OpenAIModel + `",
		"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"mode\": \"userPassthrough\", \"reason\": \"solo fresh DM, low stakes\"}"}}],
		"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	client := oa.NewClient(option.WithAPIKey("sk-test"), option.WithBaseURL(srv.URL))
	p := NewOpenAICompatible(client, OpenAIModel, "openai")
	assert.Equal(t, "openai", p.Name())

	got, err := p.Recommend(context.Background(), Request{
		AgentName:        "demo-agent",
		ChannelKind:      "slack",
		IsDirectMessage:  true,
		ParticipantCount: 1,
		ThreadDepth:      1,
		InitiatingUser:   "demo-user",
		InboundText:      "hey, can you help me draft this?",
	})
	require.NoError(t, err)
	assert.Equal(t, "userPassthrough", got.Mode)
	assert.Equal(t, "solo fresh DM, low stakes", got.Reason)
}

// TestOpenAICompatibleProvider_Recommend_WireLevelModel asserts the REQUEST
// body's "model" field carries the constant NewOpenAICompatible was
// constructed with — for both the "openai" and "openrouter" model choices,
// both reachable directly in this package since NewOpenAICompatible takes
// the model as a plain parameter (the provider dispatch that picks
// OpenAIModel vs. OpenRouterModel lives in internal/cmd/runner's
// buildIdentityRecommender, not here). A regression that reverts to a dead
// slug, or crosses the two models, fails here even though the fake server
// happily echoes back whatever canned response it's given — the assertion
// is on the outgoing request, not the response.
func TestOpenAICompatibleProvider_Recommend_WireLevelModel(t *testing.T) {
	cases := []struct {
		name         string
		model        string
		providerName string
	}{
		{name: "openai model reaches the wire", model: OpenAIModel, providerName: "openai"},
		{name: "openrouter model reaches the wire", model: OpenRouterModel, providerName: "openrouter"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var reqBody struct {
					Model string `json:"model"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&reqBody))
				gotModel = reqBody.Model

				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
					"id": "chatcmpl-1", "object": "chat.completion", "model": "` + tc.model + `",
					"choices": [{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"mode\": \"agent\", \"reason\": \"class prompt prefers agent identity\"}"}}],
					"usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20}
				}`))
			}))
			t.Cleanup(srv.Close)

			client := oa.NewClient(option.WithAPIKey("sk-test"), option.WithBaseURL(srv.URL))
			p := NewOpenAICompatible(client, tc.model, tc.providerName)

			_, err := p.Recommend(context.Background(), Request{AgentName: "demo-agent"})
			require.NoError(t, err)
			assert.Equal(t, tc.model, gotModel, "request must carry the model NewOpenAICompatible was constructed with")
		})
	}
}

// TestOpenAICompatibleProvider_implementsProvider confirms at compile time
// that *OpenAICompatibleProvider satisfies Provider (also pinned by the
// package-level compile-time check in openaicompat.go; this test documents
// the same fact from the test file for anyone scanning tests-only).
func TestOpenAICompatibleProvider_implementsProvider(t *testing.T) {
	var _ Provider = (*OpenAICompatibleProvider)(nil)
}
