package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/authzed/openagentprimitives/pkg/memory"
)

type OpenAIEmbedder struct {
	endpoint string // e.g. "https://api.openai.com" (no trailing slash)
	apiKey   string
	model    string // e.g. "text-embedding-3-small"
	client   *http.Client
}

var _ memory.EmbeddingProvider = (*OpenAIEmbedder)(nil)

type Option func(*OpenAIEmbedder)

func WithModel(model string) Option {
	return func(e *OpenAIEmbedder) { e.model = model }
}

func New(endpoint, apiKey string, opts ...Option) *OpenAIEmbedder {
	e := &OpenAIEmbedder{
		endpoint: endpoint,
		apiKey:   apiKey,
		model:    "text-embedding-3-small",
		client:   &http.Client{Timeout: 30 * time.Second},
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// embeddingRequest is the OpenAI-compatible /embeddings request body.
type embeddingRequest struct {
	// Model must be the SAME model every stored vector was produced with;
	// vectors from different models are not comparable.
	Model string `json:"model"`
	// Input is the text to embed; this API takes one string per request.
	Input string `json:"input"`
}

// embeddingResponse is the OpenAI-compatible /embeddings response body.
type embeddingResponse struct {
	// Data holds one element per input. Empty means the endpoint answered 200
	// with no vector, which callers must treat as a failure, not a zero vector.
	Data []struct {
		// Embedding is the dense vector; its length must match the pgvector
		// column's declared dimension or the insert is rejected.
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(embeddingRequest{Model: e.model, Input: text})
	if err != nil {
		return nil, fmt.Errorf("embedding: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		e.endpoint+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embedding: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding: request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("embedding: status %d: %s", resp.StatusCode, msg)
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedding: decode: %w", err)
	}
	if len(result.Data) == 0 || len(result.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embedding: empty response")
	}
	return result.Data[0].Embedding, nil
}
