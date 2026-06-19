// Package attachmentindex runs a background pass that indexes image attachments
// for OCR and semantic search.
package attachmentindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai/vision"
	"github.com/usememos/memos/store"
)

// VectorStore is the subset of *vector.AttachmentStore the runner depends on.
type VectorStore interface {
	UpsertAttachment(ctx context.Context, attachment *store.Attachment, index *store.AttachmentSearchIndex, contentSHA string) error
	DeleteAttachment(ctx context.Context, attachmentID int32) error
	ContentSHA(attachmentID int32) string
	Reconcile(ctx context.Context, validIDs map[int32]struct{}) (int, error)
	Model() string
}

// BlobReader reads an attachment's stored bytes regardless of backend.
type BlobReader interface {
	GetAttachmentBlob(attachment *store.Attachment) ([]byte, error)
}

// Runner periodically indexes image attachments into the attachment vector store.
type Runner struct {
	Store            *store.Store
	Vector           VectorStore
	Analyzer         vision.Analyzer
	BlobReader       BlobReader
	Interval         time.Duration
	VisionProviderID string
	VisionModel      string
	Prompt           string
}

// NewRunner constructs a runner. A non-positive interval falls back to 5m.
func NewRunner(s *store.Store, vstore VectorStore, analyzer vision.Analyzer, blobReader BlobReader, interval time.Duration, visionProviderID, visionModel, prompt string) *Runner {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return &Runner{Store: s, Vector: vstore, Analyzer: analyzer, BlobReader: blobReader, Interval: interval, VisionProviderID: visionProviderID, VisionModel: visionModel, Prompt: prompt}
}

// RunOnce performs a full incremental image indexing and vector reconciliation pass.
func (r *Runner) RunOnce(ctx context.Context) {
	validIDs := make(map[int32]struct{})
	indexed, err := r.runUpsertPass(ctx, validIDs)
	if err != nil {
		slog.Error("attachmentindex upsert pass failed", "err", err)
	}
	deleted, err := r.Vector.Reconcile(ctx, validIDs)
	if err != nil {
		slog.Error("attachmentindex reconcile failed", "err", err)
	}
	slog.Info("attachmentindex pass complete", "indexed", indexed, "reconciled_deleted", deleted, "valid_in_sql", len(validIDs))
}

func (r *Runner) runUpsertPass(ctx context.Context, validIDs map[int32]struct{}) (int, error) {
	const batchSize = 50
	offset := 0
	indexed := 0
	now := time.Now().Unix()

	for {
		if ctx.Err() != nil {
			return indexed, ctx.Err()
		}
		limit := batchSize
		attachments, err := r.Store.ListAttachments(ctx, &store.FindAttachment{Limit: &limit, Offset: &offset})
		if err != nil {
			return indexed, err
		}
		if len(attachments) == 0 {
			break
		}

		for _, attachment := range attachments {
			if !isImageAttachment(attachment) {
				continue
			}
			validIDs[attachment.ID] = struct{}{}
			if err := r.indexOne(ctx, attachment, now); err != nil {
				slog.Error("attachmentindex index failed", "err", err, "attachmentID", attachment.ID)
				continue
			}
			indexed++
		}

		offset += len(attachments)
	}
	return indexed, nil
}

func (r *Runner) indexOne(ctx context.Context, attachment *store.Attachment, now int64) error {
	attachmentWithBlob, err := r.Store.GetAttachment(ctx, &store.FindAttachment{ID: &attachment.ID, GetBlob: true})
	if err != nil {
		return errors.Wrap(err, "get attachment blob row")
	}
	if attachmentWithBlob == nil {
		return nil
	}
	attachment = attachmentWithBlob
	current, err := r.Store.GetAttachmentSearchIndex(ctx, attachment.ID)
	if err != nil {
		return errors.Wrap(err, "get attachment search index")
	}
	if current != nil && current.Status == store.AttachmentSearchIndexStatusFailed && current.NextRetryTs > now {
		return nil
	}
	blob, err := r.BlobReader.GetAttachmentBlob(attachment)
	if err != nil {
		return r.markFailed(ctx, attachment, current, now, err)
	}
	contentSHA := computeAttachmentContentSHA(attachment, blob, r.VisionProviderID, r.VisionModel, r.Prompt, r.Vector.Model())
	if current != nil && current.Status == store.AttachmentSearchIndexStatusReady && current.ContentSHA == contentSHA && r.Vector.ContentSHA(attachment.ID) == contentSHA {
		return nil
	}

	resp, err := r.Analyzer.Analyze(ctx, vision.Request{
		Image:       blob,
		ContentType: attachment.Type,
		Filename:    attachment.Filename,
		Model:       r.VisionModel,
		Prompt:      r.Prompt,
	})
	if err != nil {
		return r.markFailed(ctx, attachment, current, now, err)
	}
	tagsJSON, _ := json.Marshal(resp.Tags)
	objectsJSON, _ := json.Marshal(resp.Objects)
	index := &store.AttachmentSearchIndex{
		AttachmentID:     attachment.ID,
		ContentSHA:       contentSHA,
		OCRText:          resp.OCRText,
		Caption:          resp.Caption,
		TagsJSON:         string(tagsJSON),
		ObjectsJSON:      string(objectsJSON),
		Status:           store.AttachmentSearchIndexStatusReady,
		VisionProviderID: r.VisionProviderID,
		VisionModel:      r.VisionModel,
		EmbeddingModel:   r.Vector.Model(),
		IndexedTs:        now,
	}
	if err := r.Store.UpsertAttachmentSearchIndex(ctx, index); err != nil {
		return errors.Wrap(err, "upsert attachment search index")
	}
	if err := r.Vector.UpsertAttachment(ctx, attachment, index, contentSHA); err != nil {
		return errors.Wrap(err, "upsert attachment vector")
	}
	return nil
}

func (r *Runner) markFailed(ctx context.Context, attachment *store.Attachment, current *store.AttachmentSearchIndex, now int64, cause error) error {
	attempt := 1
	if current != nil {
		attempt = current.AttemptCount + 1
	}
	nextRetry := now + int64(time.Duration(attempt)*time.Minute/time.Second)
	index := &store.AttachmentSearchIndex{
		AttachmentID:     attachment.ID,
		Status:           store.AttachmentSearchIndexStatusFailed,
		Error:            cause.Error(),
		AttemptCount:     attempt,
		NextRetryTs:      nextRetry,
		VisionProviderID: r.VisionProviderID,
		VisionModel:      r.VisionModel,
		EmbeddingModel:   r.Vector.Model(),
		IndexedTs:        now,
	}
	if current != nil {
		index.ContentSHA = current.ContentSHA
		index.OCRText = current.OCRText
		index.Caption = current.Caption
		index.TagsJSON = current.TagsJSON
		index.ObjectsJSON = current.ObjectsJSON
	}
	if err := r.Store.UpsertAttachmentSearchIndex(ctx, index); err != nil {
		return errors.Wrap(err, "mark attachment search index failed")
	}
	return cause
}

// Run loops until ctx is cancelled, ticking at r.Interval.
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

func isImageAttachment(attachment *store.Attachment) bool {
	return attachment != nil && strings.HasPrefix(strings.ToLower(attachment.Type), "image/")
}

func computeAttachmentContentSHA(attachment *store.Attachment, blob []byte, visionProviderID, visionModel, prompt, embeddingModel string) string {
	h := sha256.New()
	_, _ = h.Write(blob)
	_, _ = h.Write([]byte("\x00" + attachment.Filename + "\x00" + attachment.Type + "\x00" + visionProviderID + "\x00" + visionModel + "\x00" + prompt + "\x00" + embeddingModel))
	return hex.EncodeToString(h.Sum(nil)[:16])
}
