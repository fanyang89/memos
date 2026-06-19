package memoindex_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/vector"
	"github.com/usememos/memos/server/runner/memoindex"
	"github.com/usememos/memos/store"
	storetest "github.com/usememos/memos/store/test"
)

// stubVector is a minimal VectorStore used only where the store is never
// exercised (e.g. interval defaults). Real upsert/reconcile behavior is tested
// through newRealVector + the failingVector wrapper.
type stubVector struct{}

func (stubVector) UpsertMemo(context.Context, *store.Memo, string) error      { return nil }
func (stubVector) DeleteMemo(context.Context, int32) error                    { return nil }
func (stubVector) ContentSHA(int32) string                                    { return "" }
func (stubVector) Reconcile(context.Context, map[int32]struct{}) (int, error) { return 0, nil }

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

func newRealVector(t *testing.T) *vector.Store {
	t.Helper()
	s, err := vector.NewInMemory("runner-test-model", fakeEmbedder{})
	require.NoError(t, err)
	return s
}

func TestNewRunner_DefaultInterval(t *testing.T) {
	t.Parallel()
	require.Equal(t, 5*time.Minute, memoindex.NewRunner(nil, stubVector{}, 0).Interval)
	require.Equal(t, 5*time.Minute, memoindex.NewRunner(nil, stubVector{}, -1).Interval)
	require.Equal(t, 30*time.Second, memoindex.NewRunner(nil, stubVector{}, 30*time.Second).Interval)
}

func TestRunOnce_IndexesNewMemos(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.NewTestingStore(ctx, t)
	user := createUser(t, st)

	memo1, err := st.CreateMemo(ctx, newMemo(user.ID, "first memo"))
	require.NoError(t, err)
	memo2, err := st.CreateMemo(ctx, newMemo(user.ID, "second memo"))
	require.NoError(t, err)

	vs := newRealVector(t)
	memoindex.NewRunner(st, vs, time.Minute).RunOnce(ctx)

	require.NotEqual(t, "", vs.ContentSHA(memo1.ID), "memo1 should be indexed")
	require.NotEqual(t, "", vs.ContentSHA(memo2.ID), "memo2 should be indexed")
}

func TestRunOnce_ReconcilesOrphans(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.NewTestingStore(ctx, t)
	user := createUser(t, st)

	keep1, err := st.CreateMemo(ctx, newMemo(user.ID, "keep one"))
	require.NoError(t, err)
	keep2, err := st.CreateMemo(ctx, newMemo(user.ID, "keep two"))
	require.NoError(t, err)
	orphan, err := st.CreateMemo(ctx, newMemo(user.ID, "doomed"))
	require.NoError(t, err)

	vs := newRealVector(t)
	require.NoError(t, vs.UpsertMemo(ctx, keep1, vector.ComputeContentSHA(vector.PlainText(keep1.Content))))
	require.NoError(t, vs.UpsertMemo(ctx, keep2, vector.ComputeContentSHA(vector.PlainText(keep2.Content))))
	require.NoError(t, vs.UpsertMemo(ctx, orphan, vector.ComputeContentSHA(vector.PlainText(orphan.Content))))

	// Hard-delete the orphan from SQL so the reconcile pass must remove it.
	require.NoError(t, st.DeleteMemo(ctx, &store.DeleteMemo{ID: orphan.ID}))

	memoindex.NewRunner(st, vs, time.Minute).RunOnce(ctx)

	require.NotEqual(t, "", vs.ContentSHA(keep1.ID), "keep1 still indexed")
	require.NotEqual(t, "", vs.ContentSHA(keep2.ID), "keep2 still indexed")
	require.Equal(t, "", vs.ContentSHA(orphan.ID), "orphan reconciled away")
}

func TestRunOnce_FailureIsolation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.NewTestingStore(ctx, t)
	user := createUser(t, st)

	memo1, err := st.CreateMemo(ctx, newMemo(user.ID, "ok memo one"))
	require.NoError(t, err)
	badMemo, err := st.CreateMemo(ctx, newMemo(user.ID, "bad memo"))
	require.NoError(t, err)
	memo3, err := st.CreateMemo(ctx, newMemo(user.ID, "ok memo three"))
	require.NoError(t, err)

	vs := &failingVector{inner: newRealVector(t), failOn: badMemo.ID}
	r := memoindex.NewRunner(st, vs, time.Minute)

	require.NotPanics(t, func() { r.RunOnce(ctx) })
	require.NotEqual(t, "", vs.inner.ContentSHA(memo1.ID), "memo1 still indexed despite sibling failure")
	require.NotEqual(t, "", vs.inner.ContentSHA(memo3.ID), "memo3 still indexed despite sibling failure")
}

var errInjected = errors.New("injected upsert failure")

type failingVector struct {
	inner  *vector.Store
	failOn int32
	mu     sync.Mutex
	called bool
}

func (f *failingVector) UpsertMemo(ctx context.Context, memo *store.Memo, sha string) error {
	if memo.ID == f.failOn {
		f.mu.Lock()
		f.called = true
		f.mu.Unlock()
		return errInjected
	}
	return f.inner.UpsertMemo(ctx, memo, sha)
}
func (f *failingVector) DeleteMemo(ctx context.Context, id int32) error {
	return f.inner.DeleteMemo(ctx, id)
}
func (f *failingVector) ContentSHA(id int32) string { return f.inner.ContentSHA(id) }
func (f *failingVector) Reconcile(ctx context.Context, valid map[int32]struct{}) (int, error) {
	return f.inner.Reconcile(ctx, valid)
}

func TestRun_UntilContextCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	st := storetest.NewTestingStore(ctx, t)
	vs := newRealVector(t)
	r := memoindex.NewRunner(st, vs, 10*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Run did not exit after context cancel")
	}
}

func createUser(t *testing.T, st *store.Store) *store.User {
	t.Helper()
	u, err := st.CreateUser(context.Background(), &store.User{
		Username:     "runner-tester",
		Role:         store.RoleAdmin,
		PasswordHash: "x",
	})
	require.NoError(t, err)
	return u
}

var memoSeq int32

func newMemo(creatorID int32, content string) *store.Memo {
	uid := fmt.Sprintf("memo-%d-%d", creatorID, atomic.AddInt32(&memoSeq, 1))
	return &store.Memo{CreatorID: creatorID, UID: uid, Content: content, Visibility: store.Private}
}
