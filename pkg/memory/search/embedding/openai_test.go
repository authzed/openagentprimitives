package embedding_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/authzed/openagentprimitives/pkg/memory/search/embedding"
)

func TestOpenAIEmbedder_Embed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, "text-embedding-3-small", req.Model)
		assert.Equal(t, "hello world", req.Input)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"embedding": []float32{0.1, 0.2, 0.3}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	e := embedding.New(srv.URL, "test-key")
	vec, err := e.Embed(context.Background(), "hello world")
	require.NoError(t, err)
	assert.Len(t, vec, 3)
	assert.InDelta(t, 0.1, float64(vec[0]), 0.001)
}

func TestOpenAIEmbedder_CustomModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		assert.Equal(t, "custom-model", req.Model)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"embedding": []float32{0.5}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	e := embedding.New(srv.URL, "key", embedding.WithModel("custom-model"))
	vec, err := e.Embed(context.Background(), "test")
	require.NoError(t, err)
	assert.Len(t, vec, 1)
}

func TestOpenAIEmbedder_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	e := embedding.New(srv.URL, "key")
	_, err := e.Embed(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "429")
}

func TestOpenAIEmbedder_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
	}))
	t.Cleanup(srv.Close)

	e := embedding.New(srv.URL, "key")
	_, err := e.Embed(context.Background(), "test")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty response")
}

func TestOpenAIEmbedder_NoAuthKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": []map[string]interface{}{
				{"embedding": []float32{0.1}},
			},
		})
	}))
	t.Cleanup(srv.Close)

	e := embedding.New(srv.URL, "")
	vec, err := e.Embed(context.Background(), "test")
	require.NoError(t, err)
	assert.Len(t, vec, 1)
}
