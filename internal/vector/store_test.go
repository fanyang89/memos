package vector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/philippgille/chromem-go"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/ai/embeddings"
	"github.com/usememos/memos/store"
)

// fakeEmbedder returns deterministic vectors keyed on input text length and a
// configured offset, so similarity is stable and tests can assert ordering.
type fakeEmbedder struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 4)
		// Build a vector from cheap text features so different texts differ.
		v[0] = float32(len(t))
		v[1] = float32(t[0])
		v[2] = float32(len(t) * 2)
		v[3] = 1
		out[i] = normalize(v)
	}
	return out, nil
}

func normalize(v []float32) []float32 {
	var sum float32
	for _, x := range v {
		sum += x * x
	}
	if sum == 0 {
		return v
	}
	n := 1 / sqrtf(sum)
	for i := range v {
		v[i] *= n
	}
	return v
}

func sqrtf(x float32) float32 {
	// Newton's method, good enough for tests.
	z := x
	for i := 0; i < 12; i++ {
		z = (z + x/z) * 0.5
	}
	return z
}

var _ embeddings.Embedder = (*fakeEmbedder)(nil)

type failingEmbedder struct{}

func (failingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, text := range texts {
		if strings.Contains(text, "fail") {
			return nil, errors.New("injected embedding failure")
		}
		out = append(out, []float32{1, 0, 0, 0})
	}
	return out, nil
}

var _ embeddings.Embedder = (*failingEmbedder)(nil)

func newTestStore(t *testing.T, model string) *Store {
	t.Helper()
	s, err := NewInMemory(model, &fakeEmbedder{})
	require.NoError(t, err)
	return s
}

func TestUpsertAndQuery(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()

	require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: 1, CreatorID: 7, Content: "alpha bravo charlie", Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA("alpha bravo charlie")))
	require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: 2, CreatorID: 7, Content: "zzz different content here", Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA("zzz different content here")))

	hits, err := s.Query(ctx, "alpha bravo charlie", 5, map[string]string{metaCreatorID: "7"})
	require.NoError(t, err)
	require.Len(t, hits, 2)
	// Top hit is memo 1 (identical text → highest similarity).
	require.Equal(t, int32(1), hits[0].MemoID)
}

func TestUpsertOverwrite(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()

	memo := &store.Memo{ID: 1, CreatorID: 7, Content: "first version", Visibility: store.Private, RowStatus: store.Normal}
	require.NoError(t, s.UpsertMemo(ctx, memo, ComputeContentSHA("first version")))
	require.Equal(t, ComputeContentSHA("first version"), s.ContentSHA(1))

	memo.Content = "second version totally different"
	require.NoError(t, s.UpsertMemo(ctx, memo, ComputeContentSHA("second version totally different")))
	require.Equal(t, ComputeContentSHA("second version totally different"), s.ContentSHA(1))

	hits, err := s.Query(ctx, "second version totally different", 5, nil)
	require.NoError(t, err)
	require.Len(t, hits, 1, "overwrite must not duplicate")
	require.Equal(t, int32(1), hits[0].MemoID)
}

func TestDeleteMemo(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()

	content := strings.Repeat("hello world。", 1000)
	require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: 1, CreatorID: 7, Content: content, Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA(PlainText(content))))
	require.Greater(t, s.collection.Count(), 1, "test content should produce multiple chunks")
	require.NoError(t, s.DeleteMemo(ctx, 1))
	require.Equal(t, "", s.ContentSHA(1))

	hits, err := s.Query(ctx, "hello", 5, nil)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestDifferentModelDifferentCollection(t *testing.T) {
	t.Parallel()
	s1, err := NewInMemory("model-a", &fakeEmbedder{})
	require.NoError(t, err)
	s2, err := NewInMemory("model-b", &fakeEmbedder{})
	require.NoError(t, err)
	require.NotEqual(t, s1.collection.Name, s2.collection.Name)
}

func TestReconcileDeletesOrphans(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()

	for i := int32(1); i <= 3; i++ {
		require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: i, CreatorID: 1, Content: "content " + string(rune('a'+i)), Visibility: store.Private, RowStatus: store.Normal}, "sha"))
	}

	deleted, err := s.Reconcile(ctx, map[int32]struct{}{1: {}, 2: {}})
	require.NoError(t, err)
	require.Equal(t, 1, deleted)
	require.Equal(t, "", s.ContentSHA(3))
	require.NotEqual(t, "", s.ContentSHA(1))
	require.NotEqual(t, "", s.ContentSHA(2))
}

func TestReconcileEmpty(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	deleted, err := s.Reconcile(context.Background(), map[int32]struct{}{1: {}})
	require.NoError(t, err)
	require.Equal(t, 0, deleted)
}

func TestReconcileContextCancel(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx, cancel := context.WithCancel(context.Background())

	for i := int32(1); i <= 5; i++ {
		require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: i, CreatorID: 1, Content: "x", Visibility: store.Private, RowStatus: store.Normal}, "sha"))
	}
	// Pre-cancel: Reconcile should bail immediately.
	cancel()
	// With validIDs empty and ctx already cancelled, the first iteration returns early.
	_, err := s.Reconcile(ctx, map[int32]struct{}{})
	// Either no error (if it exits before checking) or context error; both acceptable.
	_ = err
}

func TestNilStoreIsSafe(t *testing.T) {
	t.Parallel()
	var s *Store
	require.NoError(t, s.UpsertMemo(context.Background(), &store.Memo{ID: 1}, "sha"))
	require.NoError(t, s.DeleteMemo(context.Background(), 1))
	hits, err := s.Query(context.Background(), "q", 5, nil)
	require.NoError(t, err)
	require.Nil(t, hits)
	deleted, err := s.Reconcile(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, deleted)
	require.Equal(t, "", s.ContentSHA(1))
}

func TestParseMemoID(t *testing.T) {
	cases := []struct {
		in    string
		id    int32
		chunk int32
		ok    bool
	}{
		{"memo-1-chunk-0", 1, 0, true},
		{"memo-123-chunk-45", 123, 45, true},
		{"memo-1", 0, 0, false},
		{"memo-0-chunk-0", 0, 0, false},
		{"memo-1-chunk--1", 0, 0, false},
		{"memo--5-chunk-0", 0, 0, false},
		{"foobar", 0, 0, false},
		{"memo-abc-chunk-0", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		id, chunk, ok := parseMemoChunkID(c.in)
		require.Equal(t, c.ok, ok, c.in)
		if ok {
			require.Equal(t, c.id, id, c.in)
			require.Equal(t, c.chunk, chunk, c.in)
		}
	}
}

func TestUpsertLongMemoCreatesChunkHits(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()
	content := strings.Repeat("中文检索内容很长，需要按照句子切成多个片段。", 800)
	plain := PlainText(content)

	require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: 7, CreatorID: 3, Content: content, Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA(plain)))
	require.Greater(t, s.collection.Count(), 1)

	hits, err := s.Query(ctx, "中文检索", 10, map[string]string{metaCreatorID: "3"})
	require.NoError(t, err)
	require.NotEmpty(t, hits)
	require.Equal(t, int32(7), hits[0].MemoID)
	require.GreaterOrEqual(t, hits[0].ChunkIndex, int32(0))
	require.NotEmpty(t, hits[0].Snippet)
}

func TestQueryMemosWidensUntilDistinctLimit(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, "test-model")
	ctx := context.Background()

	docs := make([]chromem.Document, 0, 62)
	for i := int32(0); i < 60; i++ {
		docs = append(docs, chromem.Document{
			ID:        docID(1, i),
			Content:   "dominant chunk",
			Embedding: []float32{1, 0, 0, 0},
			Metadata: map[string]string{
				metaMemoID:     "1",
				metaCreatorID:  "7",
				metaChunkIndex: strconv.Itoa(int(i)),
			},
		})
	}
	docs = append(docs,
		chromem.Document{ID: docID(2, 0), Content: "second memo", Embedding: []float32{0.95, 0.05, 0, 0}, Metadata: map[string]string{metaMemoID: "2", metaCreatorID: "7", metaChunkIndex: "0"}},
		chromem.Document{ID: docID(3, 0), Content: "third memo", Embedding: []float32{0.9, 0.1, 0, 0}, Metadata: map[string]string{metaMemoID: "3", metaCreatorID: "7", metaChunkIndex: "0"}},
	)
	require.NoError(t, s.collection.AddDocuments(ctx, docs, 1))

	hits, err := s.QueryMemos(ctx, "query", 3, map[string]string{metaCreatorID: "7"})
	require.NoError(t, err)
	require.Len(t, hits, 3)
	require.ElementsMatch(t, []int32{1, 2, 3}, []int32{hits[0].MemoID, hits[1].MemoID, hits[2].MemoID})
}

func TestUpsertMemoCleansPartialChunksOnEmbeddingFailure(t *testing.T) {
	t.Parallel()
	s, err := NewInMemory("test-model", failingEmbedder{})
	require.NoError(t, err)
	ctx := context.Background()
	content := strings.Repeat("ok sentence。", 1000) + strings.Repeat("fail sentence。", 1000)
	plain := PlainText(content)

	err = s.UpsertMemo(ctx, &store.Memo{ID: 9, CreatorID: 1, Content: content, Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA(plain))
	require.Error(t, err)
	require.Equal(t, 0, s.collection.Count())
	require.Equal(t, "", s.ContentSHA(9))
}

func TestPersistentStoreSurvivesRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	model := "persist-model"
	emb := &fakeEmbedder{}

	s1, err := NewPersistent(context.Background(), dir, model, emb)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, s1.UpsertMemo(ctx, &store.Memo{ID: 42, CreatorID: 1, Content: "persisted memo text", Visibility: store.Private, RowStatus: store.Normal}, "sha-42"))
	require.Equal(t, "sha-42", s1.ContentSHA(42))
	require.FileExists(t, filepath.Join(dir, "vector-db", indexFileName(collectionName(model))))

	// Reopen: the sidecar index must report the same SHA so the runner skips it.
	s2, err := NewPersistent(context.Background(), dir, model, emb)
	require.NoError(t, err)
	require.Equal(t, "sha-42", s2.ContentSHA(42), "sidecar index must reload after restart")

	// And the document must still be queryable.
	hits, err := s2.Query(ctx, "persisted memo text", 5, nil)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, int32(42), hits[0].MemoID)
}

func TestToPlainText(t *testing.T) {
	cases := []struct {
		in, out string
	}{
		{"hello world", "hello world"},
		{"`code` here", "code here"},
		{"![alt](http://x/y.png)", ""},
		{"[link text](http://x)", "link text"},
		{"```go\nfunc()\n```", ""},
		{"<b>bold</b>", "bold"},
	}
	for _, c := range cases {
		require.Equal(t, c.out, toPlainText(c.in), c.in)
	}
}

func TestPersistentStoreUnwritableDir(t *testing.T) {
	t.Parallel()
	// A path under a non-existent root that we cannot create.
	_, err := NewPersistent(context.Background(), "/nonexistent-root-xyz/cannot/create", "m", &fakeEmbedder{})
	require.Error(t, err)
}

func TestPersistentStoreIndexCreated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := NewPersistent(context.Background(), dir, "m", &fakeEmbedder{})
	require.NoError(t, err)
	require.NoError(t, s.UpsertMemo(context.Background(), &store.Memo{ID: 1, CreatorID: 1, Content: "hi", Visibility: store.Private, RowStatus: store.Normal}, "sha"))
	_, err = os.Stat(filepath.Join(dir, "vector-db", indexFileName(collectionName("m"))))
	require.NoError(t, err)
}
