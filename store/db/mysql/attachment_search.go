package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/usememos/memos/store"
)

func (d *DB) UpsertAttachmentSearchIndex(ctx context.Context, upsert *store.AttachmentSearchIndex) error {
	stmt := `
		INSERT INTO attachment_search_index (
			attachment_id, content_sha, ocr_text, caption, tags_json, objects_json, status, error,
			attempt_count, next_retry_ts, vision_provider_id, vision_model, embedding_model, indexed_ts
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			content_sha = VALUES(content_sha),
			ocr_text = VALUES(ocr_text),
			caption = VALUES(caption),
			tags_json = VALUES(tags_json),
			objects_json = VALUES(objects_json),
			status = VALUES(status),
			error = VALUES(error),
			attempt_count = VALUES(attempt_count),
			next_retry_ts = VALUES(next_retry_ts),
			vision_provider_id = VALUES(vision_provider_id),
			vision_model = VALUES(vision_model),
			embedding_model = VALUES(embedding_model),
			indexed_ts = VALUES(indexed_ts)`
	_, err := d.db.ExecContext(ctx, stmt,
		upsert.AttachmentID, upsert.ContentSHA, upsert.OCRText, upsert.Caption, upsert.TagsJSON, upsert.ObjectsJSON,
		upsert.Status, upsert.Error, upsert.AttemptCount, upsert.NextRetryTs, upsert.VisionProviderID, upsert.VisionModel,
		upsert.EmbeddingModel, upsert.IndexedTs,
	)
	return err
}

func (d *DB) ListAttachmentSearchIndexes(ctx context.Context, find *store.FindAttachmentSearchIndex) ([]*store.AttachmentSearchIndex, error) {
	where, args := []string{"1 = 1"}, []any{}
	if v := find.AttachmentID; v != nil {
		where, args = append(where, "attachment_search_index.attachment_id = ?"), append(args, *v)
	}
	if v := find.CreatorID; v != nil {
		where, args = append(where, "attachment.creator_id = ?"), append(args, *v)
	}
	if v := find.Status; v != nil {
		where, args = append(where, "attachment_search_index.status = ?"), append(args, *v)
	}

	query := `
		SELECT attachment_search_index.attachment_id, attachment_search_index.content_sha, attachment_search_index.ocr_text,
			attachment_search_index.caption, attachment_search_index.tags_json, attachment_search_index.objects_json,
			attachment_search_index.status, attachment_search_index.error, attachment_search_index.attempt_count,
			attachment_search_index.next_retry_ts, attachment_search_index.vision_provider_id, attachment_search_index.vision_model,
			attachment_search_index.embedding_model, attachment_search_index.indexed_ts
		FROM attachment_search_index
		JOIN attachment ON attachment.id = attachment_search_index.attachment_id
		WHERE ` + strings.Join(where, " AND ") + `
		ORDER BY attachment_search_index.indexed_ts DESC, attachment_search_index.attachment_id DESC`
	if find.Limit != nil {
		query = fmt.Sprintf("%s LIMIT %d", query, *find.Limit)
		if find.Offset != nil {
			query = fmt.Sprintf("%s OFFSET %d", query, *find.Offset)
		}
	}

	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAttachmentSearchIndexes(rows)
}

func (d *DB) DeleteAttachmentSearchIndex(ctx context.Context, attachmentID int32) error {
	_, err := d.db.ExecContext(ctx, "DELETE FROM attachment_search_index WHERE attachment_id = ?", attachmentID)
	return err
}

func scanAttachmentSearchIndexes(rows *sql.Rows) ([]*store.AttachmentSearchIndex, error) {
	list := make([]*store.AttachmentSearchIndex, 0)
	for rows.Next() {
		index := &store.AttachmentSearchIndex{}
		if err := rows.Scan(
			&index.AttachmentID, &index.ContentSHA, &index.OCRText, &index.Caption, &index.TagsJSON, &index.ObjectsJSON,
			&index.Status, &index.Error, &index.AttemptCount, &index.NextRetryTs, &index.VisionProviderID, &index.VisionModel,
			&index.EmbeddingModel, &index.IndexedTs,
		); err != nil {
			return nil, err
		}
		list = append(list, index)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}
