package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/ai"
	embeddingsopenai "github.com/usememos/memos/internal/ai/embeddings/openai"
	"github.com/usememos/memos/internal/vector"
	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	storepb "github.com/usememos/memos/proto/gen/store"
)

// newMockEmbeddingsServer returns an httptest server that responds to
// /v1/embeddings (Ollama/OpenAI-compat) with fixed 8-dim vectors derived from
// input length, so identical-ish texts cluster.
func newMockEmbeddingsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		inputs, _ := body["input"].([]any)
		data := make([]map[string]any, 0, len(inputs))
		for i, in := range inputs {
			s, _ := in.(string)
			vec := make([]float64, 8)
			for j := range vec {
				vec[j] = float64(len(s)%8 + j + i)
			}
			data = append(data, map[string]any{"object": "embedding", "index": i, "embedding": vec})
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": "nomic-embed-text"}))
	}))
}

// injectVectorStore wires a real in-memory vector store onto the test service,
// backed by an embedder that points at the given mock server.
func injectVectorStore(t *testing.T, ts *TestService, endpoint string) *vector.Store {
	t.Helper()
	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: endpoint,
		APIKey:   "sk-test",
	}, "nomic-embed-text")
	require.NoError(t, err)
	vs, err := vector.NewInMemory("nomic-embed-text", embedder)
	require.NoError(t, err)
	ts.Service.VectorStore = vs
	return vs
}

func waitForIndex(t *testing.T, vs *vector.Store, memoID int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if vs.ContentSHA(memoID) != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("memo %d not indexed within %s", memoID, timeout)
}

func TestSearchMemos_NotConfigured(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()
	// VectorStore left nil.

	user, err := ts.CreateRegularUser(ctx, "alice")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)

	_, err = ts.Service.SearchMemos(userCtx, &v1pb.SearchMemosRequest{Query: "anything"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "FailedPrecondition")
}

func TestSearchMemos_IndexHookRoundTrip(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()

	mock := newMockEmbeddingsServer(t)
	defer mock.Close()
	vs := injectVectorStore(t, ts, mock.URL)

	// Persist an embedding config + provider so SearchMemos validation passes.
	_, err := ts.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
		Key: storepb.InstanceSettingKey_AI,
		Value: &storepb.InstanceSetting_AiSetting{
			AiSetting: &storepb.InstanceAISetting{
				Providers: []*storepb.AIProviderConfig{{
					Id:       "openai-main",
					Title:    "OpenAI",
					Type:     storepb.AIProviderType_OPENAI,
					Endpoint: mock.URL,
					ApiKey:   "sk-test",
				}},
				Embedding: &storepb.EmbeddingConfig{
					ProviderId: "openai-main",
					Model:      "nomic-embed-text",
				},
			},
		},
	})
	require.NoError(t, err)

	user, err := ts.CreateRegularUser(ctx, "alice")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)

	// CreateMemo must trigger the async index hook.
	created, err := ts.Service.CreateMemo(userCtx, &v1pb.CreateMemoRequest{
		Memo: &v1pb.Memo{Content: "semantic search notes about golang", Visibility: v1pb.Visibility_PRIVATE},
	})
	require.NoError(t, err)

	memoID := memoIDFromName(ctx, t, ts, created.Name)
	waitForIndex(t, vs, memoID, 2*time.Second)

	// SearchMemos should surface the created memo.
	resp, err := ts.Service.SearchMemos(userCtx, &v1pb.SearchMemosRequest{Query: "golang notes"})
	require.NoError(t, err)
	require.Len(t, resp.GetResults(), 1)
	require.Equal(t, created.Name, resp.GetResults()[0].GetMemo())

	// DeleteMemo must remove it from the index synchronously.
	_, err = ts.Service.DeleteMemo(userCtx, &v1pb.DeleteMemoRequest{Name: created.Name})
	require.NoError(t, err)

	resp, err = ts.Service.SearchMemos(userCtx, &v1pb.SearchMemosRequest{Query: "golang notes"})
	require.NoError(t, err)
	require.Empty(t, resp.GetResults(), "deleted memo must not appear in search")
}

func TestSearchMemos_RejectsContentFilter(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()
	mock := newMockEmbeddingsServer(t)
	defer mock.Close()
	injectVectorStore(t, ts, mock.URL)

	user, err := ts.CreateRegularUser(ctx, "bob")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)

	_, err = ts.Service.SearchMemos(userCtx, &v1pb.SearchMemosRequest{
		Query:  "x",
		Filter: `content.contains("y")`,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "InvalidArgument")
}
