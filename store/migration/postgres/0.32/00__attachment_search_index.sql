CREATE TABLE attachment_search_index (
  attachment_id INTEGER PRIMARY KEY,
  content_sha TEXT NOT NULL DEFAULT '',
  ocr_text TEXT NOT NULL DEFAULT '',
  caption TEXT NOT NULL DEFAULT '',
  tags_json TEXT NOT NULL DEFAULT '[]',
  objects_json TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'PENDING',
  error TEXT NOT NULL DEFAULT '',
  attempt_count INTEGER NOT NULL DEFAULT 0,
  next_retry_ts BIGINT NOT NULL DEFAULT 0,
  vision_provider_id TEXT NOT NULL DEFAULT '',
  vision_model TEXT NOT NULL DEFAULT '',
  embedding_model TEXT NOT NULL DEFAULT '',
  indexed_ts BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX idx_attachment_search_index_status_next_retry_ts ON attachment_search_index(status, next_retry_ts);
