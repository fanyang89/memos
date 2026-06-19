CREATE TABLE `attachment_search_index` (
  `attachment_id` INT NOT NULL PRIMARY KEY,
  `content_sha` VARCHAR(256) NOT NULL DEFAULT '',
  `ocr_text` LONGTEXT NOT NULL,
  `caption` LONGTEXT NOT NULL,
  `tags_json` LONGTEXT NOT NULL,
  `objects_json` LONGTEXT NOT NULL,
  `status` VARCHAR(64) NOT NULL DEFAULT 'PENDING',
  `error` LONGTEXT NOT NULL,
  `attempt_count` INT NOT NULL DEFAULT 0,
  `next_retry_ts` BIGINT NOT NULL DEFAULT 0,
  `vision_provider_id` VARCHAR(256) NOT NULL DEFAULT '',
  `vision_model` VARCHAR(256) NOT NULL DEFAULT '',
  `embedding_model` VARCHAR(256) NOT NULL DEFAULT '',
  `indexed_ts` BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX `idx_attachment_search_index_status_next_retry_ts` ON `attachment_search_index`(`status`, `next_retry_ts`);
