package store

import (
	"context"
	"strings"
)

const (
	// AttachmentSearchIndexStatusPending marks an image that still needs indexing.
	AttachmentSearchIndexStatusPending = "PENDING"
	// AttachmentSearchIndexStatusReady marks an image whose text and vectors are searchable.
	AttachmentSearchIndexStatusReady = "READY"
	// AttachmentSearchIndexStatusFailed marks an image indexing failure eligible for retry.
	AttachmentSearchIndexStatusFailed = "FAILED"
)

// AttachmentSearchIndex stores OCR and visual-description text for one attachment.
type AttachmentSearchIndex struct {
	AttachmentID     int32
	ContentSHA       string
	OCRText          string
	Caption          string
	TagsJSON         string
	ObjectsJSON      string
	Status           string
	Error            string
	AttemptCount     int
	NextRetryTs      int64
	VisionProviderID string
	VisionModel      string
	EmbeddingModel   string
	IndexedTs        int64
}

// FindAttachmentSearchIndex filters attachment search index rows.
type FindAttachmentSearchIndex struct {
	AttachmentID *int32
	CreatorID    *int32
	Status       *string
	Limit        *int
	Offset       *int
}

// AttachmentTextSearchResult is a lexical attachment search hit.
type AttachmentTextSearchResult struct {
	AttachmentID int32
	TextScore    float32
	Snippet      string
}

// UpsertAttachmentSearchIndex creates or replaces the search index row for an attachment.
func (s *Store) UpsertAttachmentSearchIndex(ctx context.Context, upsert *AttachmentSearchIndex) error {
	return s.driver.UpsertAttachmentSearchIndex(ctx, upsert)
}

// ListAttachmentSearchIndexes lists attachment search index rows.
func (s *Store) ListAttachmentSearchIndexes(ctx context.Context, find *FindAttachmentSearchIndex) ([]*AttachmentSearchIndex, error) {
	return s.driver.ListAttachmentSearchIndexes(ctx, find)
}

// GetAttachmentSearchIndex gets the search index row for an attachment.
func (s *Store) GetAttachmentSearchIndex(ctx context.Context, attachmentID int32) (*AttachmentSearchIndex, error) {
	rows, err := s.ListAttachmentSearchIndexes(ctx, &FindAttachmentSearchIndex{AttachmentID: &attachmentID})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// DeleteAttachmentSearchIndex deletes the search index row for an attachment.
func (s *Store) DeleteAttachmentSearchIndex(ctx context.Context, attachmentID int32) error {
	return s.driver.DeleteAttachmentSearchIndex(ctx, attachmentID)
}

// SearchAttachmentText performs a small in-process lexical ranking over indexed image text.
func (s *Store) SearchAttachmentText(ctx context.Context, creatorID int32, query string, limit int) ([]*AttachmentTextSearchResult, error) {
	query = strings.TrimSpace(strings.ToLower(query))
	if query == "" || limit <= 0 {
		return nil, nil
	}
	status := AttachmentSearchIndexStatusReady
	rows, err := s.ListAttachmentSearchIndexes(ctx, &FindAttachmentSearchIndex{CreatorID: &creatorID, Status: &status})
	if err != nil {
		return nil, err
	}
	terms := strings.Fields(query)
	results := make([]*AttachmentTextSearchResult, 0, limit)
	for _, row := range rows {
		text := strings.ToLower(strings.Join([]string{row.OCRText, row.Caption, row.TagsJSON, row.ObjectsJSON}, " "))
		score := float32(0)
		if strings.Contains(text, query) {
			score += 4
		}
		for _, term := range terms {
			if strings.Contains(text, term) {
				score += 1
			}
		}
		if score == 0 {
			continue
		}
		results = append(results, &AttachmentTextSearchResult{AttachmentID: row.AttachmentID, TextScore: score, Snippet: AttachmentSearchSnippet(row, query)})
	}
	sortAttachmentTextResults(results)
	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// AttachmentSearchSnippet returns a compact snippet from the index row.
func AttachmentSearchSnippet(row *AttachmentSearchIndex, query string) string {
	_ = query
	text := strings.Join(strings.Fields(strings.Join([]string{row.OCRText, row.Caption}, " ")), " ")
	if len([]rune(text)) <= 240 {
		return text
	}
	return string([]rune(text)[:240])
}

func sortAttachmentTextResults(results []*AttachmentTextSearchResult) {
	for i := 1; i < len(results); i++ {
		current := results[i]
		j := i - 1
		for j >= 0 && results[j].TextScore < current.TextScore {
			results[j+1] = results[j]
			j--
		}
		results[j+1] = current
	}
}
