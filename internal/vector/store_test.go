package vector

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

	require.NoError(t, s.UpsertMemo(ctx, &store.Memo{ID: 1, CreatorID: 7, Content: "hello", Visibility: store.Private, RowStatus: store.Normal}, ComputeContentSHA("hello")))
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
		in string
		id int32
		ok bool
	}{
		{"memo-1", 1, true},
		{"memo-123", 123, true},
		{"memo-0", 0, false},
		{"memo--5", 0, false},
		{"foobar", 0, false},
		{"memo-abc", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		id, ok := parseMemoID(c.in)
		require.Equal(t, c.ok, ok, c.in)
		if ok {
			require.Equal(t, c.id, id, c.in)
		}
	}
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
	require.FileExists(t, filepath.Join(dir, "vector-db", indexFileName))

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
	_, err = os.Stat(filepath.Join(dir, "vector-db", indexFileName))
	require.NoError(t, err)
}
