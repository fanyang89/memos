// Package memoindex runs a background pass that backfills missing memo
// embeddings and reconciles the vector store against the SQL store (removing
// embeddings whose memos no longer exist).
package memoindex

import (
	"context"
	"log/slog"
	"time"

	"github.com/usememos/memos/internal/vector"
	"github.com/usememos/memos/store"
)

// VectorStore is the subset of *vector.Store the runner depends on.
// Declared as an interface to ease testing with fakes.
type VectorStore interface {
	UpsertMemo(ctx context.Context, memo *store.Memo, contentSHA string) error
	DeleteMemo(ctx context.Context, memoID int32) error
	ContentSHA(memoID int32) string
	Reconcile(ctx context.Context, validIDs map[int32]struct{}) (int, error)
}

// Runner periodically indexes memos into the vector store.
type Runner struct {
	Store    *store.Store
	Vector   VectorStore
	Interval time.Duration
}

// NewRunner constructs a runner. A non-positive interval falls back to 5m.
func NewRunner(s *store.Store, vstore VectorStore, interval time.Duration) *Runner {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Runner{Store: s, Vector: vstore, Interval: interval}
}

// RunOnce performs a full incremental upsert + GC reconciliation pass.
// Order matters: upsert first (so newly-created memos are indexed), then
// reconcile (so deletes win when they happened in the same window).
func (r *Runner) RunOnce(ctx context.Context) {
	validIDs := make(map[int32]struct{})
	upsertCount, err := r.runUpsertPass(ctx, validIDs)
	if err != nil {
		slog.Error("memoindex upsert pass failed", "err", err)
	}
	deletedCount, err := r.Vector.Reconcile(ctx, validIDs)
	if err != nil {
		slog.Error("memoindex reconcile failed", "err", err)
	}
	slog.Info("memoindex pass complete",
		"upserted", upsertCount,
		"reconciled_deleted", deletedCount,
		"valid_in_sql", len(validIDs),
	)
}

// runUpsertPass lists all memos in batches, computing content_sha diff and
// upserting changed ones. Each seen memo's ID is added to validIDs for the
// subsequent Reconcile pass — even when content_sha matches and no upsert is
// needed, so reconcile does not delete still-valid memos.
func (r *Runner) runUpsertPass(ctx context.Context, validIDs map[int32]struct{}) (int, error) {
	const batchSize = 100
	offset := 0
	upserted := 0

	for {
		if ctx.Err() != nil {
			return upserted, ctx.Err()
		}
		limit := batchSize
		memos, err := r.Store.ListMemos(ctx, &store.FindMemo{
			Limit:  &limit,
			Offset: &offset,
		})
		if err != nil {
			return upserted, err
		}
		if len(memos) == 0 {
			break
		}

		for _, memo := range memos {
			validIDs[memo.ID] = struct{}{}
			sha := vector.ComputeContentSHA(vector.PlainText(memo.Content))
			if r.Vector.ContentSHA(memo.ID) == sha {
				continue
			}
			if err := r.Vector.UpsertMemo(ctx, memo, sha); err != nil {
				slog.Error("memoindex upsert failed", "err", err, "memoID", memo.ID)
				continue
			}
			upserted++
		}

		offset += len(memos)
	}
	return upserted, nil
}

// Run loops until ctx is cancelled, ticking at r.Interval. It does not run a
// pass immediately; the caller is expected to invoke RunOnce synchronously on
// startup if a first pass is wanted before the first tick.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}
