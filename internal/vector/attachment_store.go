package vector

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/philippgille/chromem-go"
	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai/embeddings"
	"github.com/usememos/memos/store"
)

// ScoredAttachment is a query hit: an attachment ID and its best matching chunk.
type ScoredAttachment struct {
	AttachmentID int32
	ChunkIndex   int32
	Similarity   float32
	Snippet      string
}

// AttachmentStore wraps a chromem-go collection for attachment image search embeddings.
type AttachmentStore struct {
	collection *chromem.Collection
	model      string
	chunker    *chunker
	embedder   embeddings.Embedder

	index     map[int32]string
	indexMu   sync.Mutex
	indexPath string
}

// NewPersistentAttachmentStore opens a persistent attachment vector collection.
func NewPersistentAttachmentStore(_ context.Context, dataDir, model string, embedder embeddings.Embedder) (*AttachmentStore, error) {
	if dataDir == "" {
		return nil, errors.New("dataDir is required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if embedder == nil {
		return nil, errors.New("embedder is required")
	}
	dbPath := filepath.Join(dataDir, "vector-db")
	db, err := chromem.NewPersistentDB(dbPath, true)
	if err != nil {
		return nil, errors.Wrap(err, "open chromem-go persistent DB")
	}
	return finalizeAttachmentStore(db, dbPath, model, embedder, true)
}

// NewInMemoryAttachmentStore creates an in-memory attachment vector store for tests.
func NewInMemoryAttachmentStore(model string, embedder embeddings.Embedder) (*AttachmentStore, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if embedder == nil {
		return nil, errors.New("embedder is required")
	}
	return finalizeAttachmentStore(chromem.NewDB(), "", model, embedder, false)
}

func finalizeAttachmentStore(db *chromem.DB, dbPath, model string, embedder embeddings.Embedder, persistent bool) (*AttachmentStore, error) {
	chunker, err := newChunker(model)
	if err != nil {
		return nil, errors.Wrap(err, "create attachment chunker")
	}
	collection, err := db.GetOrCreateCollection(attachmentCollectionName(model), nil, embeddingFunc(embedder))
	if err != nil {
		return nil, errors.Wrap(err, "get or create chromem-go collection")
	}
	s := &AttachmentStore{collection: collection, model: model, chunker: chunker, embedder: embedder, index: make(map[int32]string)}
	if persistent {
		s.indexPath = filepath.Join(dbPath, "attachment-index-"+sanitizeForPath(collection.Name)+".gob")
		if err := s.loadIndex(); err != nil {
			return nil, errors.Wrap(err, "load attachment sidecar index")
		}
	}
	return s, nil
}

func attachmentCollectionName(model string) string {
	return "attachments-" + chunkStrategyVersion + "-" + sanitizeForPath(model)
}

// AttachmentSearchText builds the text embedded for an image attachment.
func AttachmentSearchText(attachment *store.Attachment, index *store.AttachmentSearchIndex) string {
	if attachment == nil || index == nil {
		return ""
	}
	parts := []string{
		"filename: " + attachment.Filename,
		"mime_type: " + attachment.Type,
		"ocr: " + index.OCRText,
		"caption: " + index.Caption,
		"tags: " + index.TagsJSON,
		"objects: " + index.ObjectsJSON,
	}
	return strings.Join(strings.Fields(strings.Join(parts, "\n")), " ")
}

// UpsertAttachment adds or replaces chunk embeddings for a single attachment.
func (s *AttachmentStore) UpsertAttachment(ctx context.Context, attachment *store.Attachment, index *store.AttachmentSearchIndex, contentSHA string) error {
	if s == nil || s.collection == nil {
		return nil
	}
	if attachment == nil {
		return errors.New("attachment is required")
	}
	text := AttachmentSearchText(attachment, index)
	chunks, err := s.chunker.split(text)
	if err != nil {
		return errors.Wrapf(err, "split attachment %d", attachment.ID)
	}
	if err := s.collection.Delete(ctx, map[string]string{metaAttachmentID: strconv.Itoa(int(attachment.ID))}, nil); err != nil {
		return errors.Wrapf(err, "delete prior embeddings for attachment %d", attachment.ID)
	}
	if len(chunks) == 0 {
		s.setSHA(attachment.ID, contentSHA)
		return nil
	}
	docs := make([]chromem.Document, 0, len(chunks))
	for _, chunk := range chunks {
		metadata := map[string]string{
			metaAttachmentID: strconv.Itoa(int(attachment.ID)),
			metaCreatorID:    strconv.Itoa(int(attachment.CreatorID)),
			metaContentSHA:   contentSHA,
			metaChunkIndex:   strconv.Itoa(int(chunk.Index)),
			metaChunkCount:   strconv.Itoa(len(chunks)),
			metaMimeType:     attachment.Type,
			metaFilename:     attachment.Filename,
		}
		if attachment.MemoID != nil {
			metadata[metaMemoID] = strconv.Itoa(int(*attachment.MemoID))
		}
		docs = append(docs, chromem.Document{ID: attachmentDocID(attachment.ID, chunk.Index), Content: chunk.Text, Metadata: metadata})
	}
	if err := s.collection.AddDocuments(ctx, docs, runtime.NumCPU()); err != nil {
		_ = s.collection.Delete(ctx, map[string]string{metaAttachmentID: strconv.Itoa(int(attachment.ID))}, nil)
		return errors.Wrapf(err, "add embeddings for attachment %d", attachment.ID)
	}
	s.setSHA(attachment.ID, contentSHA)
	return nil
}

// DeleteAttachment removes all chunks for an attachment.
func (s *AttachmentStore) DeleteAttachment(ctx context.Context, attachmentID int32) error {
	if s == nil || s.collection == nil {
		return nil
	}
	if err := s.collection.Delete(ctx, map[string]string{metaAttachmentID: strconv.Itoa(int(attachmentID))}, nil); err != nil {
		return errors.Wrapf(err, "delete embedding for attachment %d", attachmentID)
	}
	s.setSHA(attachmentID, "")
	return nil
}

// QueryAttachments returns at most attachmentLimit distinct attachment hits.
func (s *AttachmentStore) QueryAttachments(ctx context.Context, queryText string, attachmentLimit int, where map[string]string) ([]ScoredAttachment, error) {
	if s == nil || s.collection == nil || attachmentLimit <= 0 {
		return nil, nil
	}
	queryVector, err := s.embedQuery(ctx, queryText)
	if err != nil {
		return nil, err
	}
	count := s.collection.Count()
	if count == 0 {
		return nil, nil
	}
	nResults := attachmentLimit * 10
	if nResults < 50 {
		nResults = 50
	}
	if nResults > count {
		nResults = count
	}
	for {
		hits, err := s.queryChunks(ctx, queryVector, nResults, where)
		if err != nil {
			return nil, err
		}
		distinct := distinctAttachmentHits(hits, attachmentLimit)
		if len(distinct) >= attachmentLimit || nResults >= count || len(hits) < nResults {
			return distinct, nil
		}
		nResults *= 2
		if nResults > count {
			nResults = count
		}
	}
}

func (s *AttachmentStore) embedQuery(ctx context.Context, queryText string) ([]float32, error) {
	vecs, err := s.embedder.Embed(ctx, []string{queryText})
	if err != nil {
		return nil, errors.Wrap(err, "embed attachment query")
	}
	if len(vecs) != 1 {
		return nil, errors.Errorf("expected 1 query vector, got %d", len(vecs))
	}
	return vecs[0], nil
}

func (s *AttachmentStore) queryChunks(ctx context.Context, queryVector []float32, topK int, where map[string]string) ([]ScoredAttachment, error) {
	count := s.collection.Count()
	if count == 0 {
		return nil, nil
	}
	if topK > count {
		topK = count
	}
	results, err := s.collection.QueryEmbedding(ctx, queryVector, topK, where, nil)
	if err != nil {
		return nil, errors.Wrap(err, "query chromem-go attachment collection")
	}
	out := make([]ScoredAttachment, 0, len(results))
	for _, r := range results {
		attachmentID, chunkIndex, ok := parseAttachmentChunk(r.ID, r.Metadata)
		if !ok {
			continue
		}
		out = append(out, ScoredAttachment{AttachmentID: attachmentID, ChunkIndex: chunkIndex, Similarity: r.Similarity, Snippet: snippet(r.Content)})
	}
	return out, nil
}

func distinctAttachmentHits(hits []ScoredAttachment, limit int) []ScoredAttachment {
	seen := make(map[int32]struct{}, limit)
	distinct := make([]ScoredAttachment, 0, limit)
	for _, hit := range hits {
		if _, ok := seen[hit.AttachmentID]; ok {
			continue
		}
		seen[hit.AttachmentID] = struct{}{}
		distinct = append(distinct, hit)
		if len(distinct) >= limit {
			break
		}
	}
	return distinct
}

// ContentSHA returns the last-indexed content SHA for an attachment.
func (s *AttachmentStore) ContentSHA(attachmentID int32) string {
	if s == nil {
		return ""
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	sha, ok := parseIndexValue(s.index[attachmentID])
	if !ok {
		return ""
	}
	return sha
}

// Reconcile deletes vectors whose attachment_id is not in validIDs.
func (s *AttachmentStore) Reconcile(ctx context.Context, validIDs map[int32]struct{}) (int, error) {
	if s == nil || s.collection == nil {
		return 0, nil
	}
	s.indexMu.Lock()
	indexed := make([]int32, 0, len(s.index))
	for id := range s.index {
		indexed = append(indexed, id)
	}
	s.indexMu.Unlock()
	deleted := 0
	for _, attachmentID := range indexed {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		if _, exists := validIDs[attachmentID]; exists {
			continue
		}
		if err := s.collection.Delete(ctx, map[string]string{metaAttachmentID: strconv.Itoa(int(attachmentID))}, nil); err != nil {
			return deleted, errors.Wrapf(err, "delete orphan attachment_id=%d", attachmentID)
		}
		s.setSHA(attachmentID, "")
		deleted++
	}
	return deleted, nil
}

// Model returns the embedding model this store is bound to.
func (s *AttachmentStore) Model() string {
	if s == nil {
		return ""
	}
	return s.model
}

func (s *AttachmentStore) setSHA(attachmentID int32, sha string) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if sha == "" {
		delete(s.index, attachmentID)
	} else {
		s.index[attachmentID] = indexValue(sha)
	}
	if s.indexPath != "" {
		_ = s.persistIndexLocked()
	}
}

func (s *AttachmentStore) loadIndex() error {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	data, err := os.ReadFile(s.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var idx map[int32]string
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&idx); err != nil {
		return errors.Wrap(err, "decode attachment sidecar index")
	}
	if idx == nil {
		idx = make(map[int32]string)
	}
	s.index = idx
	return nil
}

func (s *AttachmentStore) persistIndexLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.indexPath), 0o700); err != nil {
		return err
	}
	f, err := os.Create(s.indexPath)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewEncoder(f).Encode(s.index)
}

const (
	metaAttachmentID = "attachment_id"
	metaMimeType     = "mime_type"
	metaFilename     = "filename"
)

func attachmentDocID(id, chunkIndex int32) string {
	return fmt.Sprintf("attachment-%d-chunk-%d", id, chunkIndex)
}

func parseAttachmentChunk(docID string, metadata map[string]string) (int32, int32, bool) {
	if attachmentID, err := strconv.ParseInt(metadata[metaAttachmentID], 10, 32); err == nil && attachmentID > 0 {
		chunkIndex, _ := strconv.ParseInt(metadata[metaChunkIndex], 10, 32)
		if chunkIndex < 0 {
			chunkIndex = 0
		}
		return int32(attachmentID), int32(chunkIndex), true
	}
	return parseAttachmentChunkID(docID)
}

func parseAttachmentChunkID(docID string) (int32, int32, bool) {
	const prefix = "attachment-"
	const infix = "-chunk-"
	if !strings.HasPrefix(docID, prefix) {
		return 0, 0, false
	}
	parts := strings.SplitN(docID[len(prefix):], infix, 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	attachmentID, err := strconv.ParseInt(parts[0], 10, 32)
	if err != nil || attachmentID <= 0 {
		return 0, 0, false
	}
	chunkIndex, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || chunkIndex < 0 {
		return 0, 0, false
	}
	return int32(attachmentID), int32(chunkIndex), true
}
