package openai_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/ai"
	embeddingsopenai "github.com/usememos/memos/internal/ai/embeddings/openai"
)

func TestEmbed(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "text-embedding-3-small", body["model"])
		require.Equal(t, []any{"hello world", "second text"}, body["input"])

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"object": "embedding", "index": 0, "embedding": []float64{0.1, 0.2, 0.3}},
				{"object": "embedding", "index": 1, "embedding": []float64{0.4, 0.5, 0.6}},
			},
			"model": "text-embedding-3-small",
		}))
	}))
	defer server.Close()

	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: server.URL,
		APIKey:   "test-key",
	}, "text-embedding-3-small", openaioption.WithHTTPClient(server.Client()))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	vecs, err := embedder.Embed(ctx, []string{"hello world", "second text"})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	require.Equal(t, []float32{0.1, 0.2, 0.3}, vecs[0])
	require.Equal(t, []float32{0.4, 0.5, 0.6}, vecs[1])
}

func TestNew_DefaultEndpointWhenBlank(t *testing.T) {
	t.Parallel()

	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:   ai.ProviderOpenAI,
		APIKey: "test-key",
	}, "text-embedding-3-small")
	require.NoError(t, err)
	require.NotNil(t, embedder)
}

func TestNew_InvalidEndpoint(t *testing.T) {
	t.Parallel()

	// Whitespace falls back to the default endpoint (matches stt/openai normalizeEndpoint).
	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: "   ",
		APIKey:   "test-key",
	}, "text-embedding-3-small")
	require.NoError(t, err)
	require.NotNil(t, embedder)

	// Genuinely malformed URL must error.
	_, err = embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: "ht!tp://%%bad",
		APIKey:   "test-key",
	}, "text-embedding-3-small")
	require.Error(t, err)
}

func TestNew_EmptyAPIKey(t *testing.T) {
	t.Parallel()

	_, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: "https://api.openai.com/v1",
		APIKey:   "",
	}, "text-embedding-3-small")
	require.Error(t, err)
}

func TestNew_EmptyModel(t *testing.T) {
	t.Parallel()

	_, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:   ai.ProviderOpenAI,
		APIKey: "test-key",
	}, "")
	require.Error(t, err)
}

func TestEmbed_ServerError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: server.URL,
		APIKey:   "test-key",
	}, "text-embedding-3-small", openaioption.WithHTTPClient(server.Client()))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = embedder.Embed(ctx, []string{"hello"})
	require.Error(t, err)
}

func TestEmbed_EmptyInput(t *testing.T) {
	t.Parallel()

	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:   ai.ProviderOpenAI,
		APIKey: "test-key",
	}, "text-embedding-3-small")
	require.NoError(t, err)

	vecs, err := embedder.Embed(context.Background(), nil)
	require.NoError(t, err)
	require.Nil(t, vecs)
}
