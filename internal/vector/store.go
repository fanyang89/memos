// Package vector wraps chromem-go to provide memo embedding storage for
// semantic search.
//
// chromem-go v0.7.0 exposes no way to enumerate the document IDs in a
// collection (no ListIDs equivalent). To support orphan reconciliation, this
// package maintains a sidecar index file (gob-encoded) that mirrors the set of
// indexed memo IDs and their last-indexed content hash. The index is loaded
// when the store opens and rewritten after every mutating operation, so it
// survives restarts alongside the chromem-go persistent DB.
package vector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/philippgille/chromem-go"
	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai/embeddings"
	"github.com/usememos/memos/store"
)

// ScoredMemo is a query hit: a memo ID and its cosine similarity.
type ScoredMemo struct {
	MemoID     int32
	Similarity float32
}

// Store wraps a chromem-go collection for memo embeddings.
type Store struct {
	db         *chromem.DB
	collection *chromem.Collection
	model      string

	// index mirrors the memo IDs present in the collection (key) and their
	// last-indexed content SHA (value), since chromem-go v0.7.0 cannot list
	// document IDs. Persisted at indexPath.
	index     map[int32]string
	indexMu   sync.Mutex
	indexPath string
}

// NewPersistent opens (or creates) a chromem-go persistent DB at
// <dataDir>/vector-db and prepares a collection named "memos-<model>".
// Changing the model creates a new collection; the caller should run a
// backfill (see server/runner/memoindex) after a model change.
func NewPersistent(_ context.Context, dataDir, model string, embedder embeddings.Embedder) (*Store, error) {
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
	return finalizeStore(db, dbPath, model, embedder, true)
}

// NewInMemory creates a non-persistent store backed by an in-memory chromem-go
// DB. Intended for tests.
func NewInMemory(model string, embedder embeddings.Embedder) (*Store, error) {
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("model is required")
	}
	if embedder == nil {
		return nil, errors.New("embedder is required")
	}
	return finalizeStore(chromem.NewDB(), "", model, embedder, false)
}

func finalizeStore(db *chromem.DB, dbPath, model string, embedder embeddings.Embedder, persistent bool) (*Store, error) {
	collection, err := db.GetOrCreateCollection(collectionName(model), nil, embeddingFunc(embedder))
	if err != nil {
		return nil, errors.Wrap(err, "get or create chromem-go collection")
	}
	s := &Store{
		db:         db,
		collection: collection,
		model:      model,
		index:      make(map[int32]string),
	}
	if persistent {
		s.indexPath = filepath.Join(dbPath, indexFileName)
		if err := s.loadIndex(); err != nil {
			return nil, errors.Wrap(err, "load sidecar index")
		}
	}
	return s, nil
}

const indexFileName = "memo-index.gob"

func collectionName(model string) string {
	return "memos-" + sanitizeForPath(model)
}

func sanitizeForPath(s string) string {
	r := strings.NewReplacer("/", "_", "\\", "_", ":", "_", string(filepath.Separator), "_")
	return r.Replace(s)
}

// embeddingFunc adapts our batch Embedder to chromem-go's single-text signature.
func embeddingFunc(e embeddings.Embedder) chromem.EmbeddingFunc {
	return func(ctx context.Context, text string) ([]float32, error) {
		vecs, err := e.Embed(ctx, []string{text})
		if err != nil {
			return nil, err
		}
		if len(vecs) != 1 {
			return nil, errors.Errorf("expected 1 vector, got %d", len(vecs))
		}
		return vecs[0], nil
	}
}

// UpsertMemo adds or replaces the embedding for a single memo.
// ID convention: "memo-{id}". Metadata carries creator_id/visibility/row_status/content_sha.
func (s *Store) UpsertMemo(ctx context.Context, memo *store.Memo, contentSHA string) error {
	if s == nil || s.collection == nil {
		return nil
	}
	if memo == nil {
		return errors.New("memo is required")
	}
	plainText := toPlainText(memo.Content)
	// Remove any prior copy so the document is replaced cleanly.
	if err := s.collection.Delete(ctx, map[string]string{metaMemoID: strconv.Itoa(int(memo.ID))}, nil); err != nil {
		return errors.Wrapf(err, "delete prior embedding for memo %d", memo.ID)
	}
	doc := chromem.Document{
		ID:      docID(memo.ID),
		Content: plainText,
		Metadata: map[string]string{
			metaMemoID:     strconv.Itoa(int(memo.ID)),
			metaCreatorID:  strconv.Itoa(int(memo.CreatorID)),
			metaVisibility: memo.Visibility.String(),
			metaRowStatus:  memo.RowStatus.String(),
			metaContentSHA: contentSHA,
		},
	}
	if err := s.collection.AddDocuments(ctx, []chromem.Document{doc}, runtime.NumCPU()); err != nil {
		return errors.Wrapf(err, "add embedding for memo %d", memo.ID)
	}
	s.setSHA(memo.ID, contentSHA)
	return nil
}

// DeleteMemo removes the embedding for a memo.
func (s *Store) DeleteMemo(ctx context.Context, memoID int32) error {
	if s == nil || s.collection == nil {
		return nil
	}
	if err := s.collection.Delete(ctx, map[string]string{metaMemoID: strconv.Itoa(int(memoID))}, nil); err != nil {
		return errors.Wrapf(err, "delete embedding for memo %d", memoID)
	}
	s.setSHA(memoID, "")
	return nil
}

// Query returns at most topK memo IDs ranked by cosine similarity,
// pre-filtered by the given where clause (e.g. {"creator_id": "12"}).
func (s *Store) Query(ctx context.Context, queryText string, topK int, where map[string]string) ([]ScoredMemo, error) {
	if s == nil || s.collection == nil {
		return nil, nil
	}
	if topK <= 0 {
		return nil, nil
	}
	// chromem-go requires nResults <= len(documents); clamp to the collection size,
	// and short-circuit when the collection is empty (Query would otherwise error).
	count := s.collection.Count()
	if count == 0 {
		return nil, nil
	}
	if topK > count {
		topK = count
	}
	results, err := s.collection.Query(ctx, queryText, topK, where, nil)
	if err != nil {
		return nil, errors.Wrap(err, "query chromem-go collection")
	}
	out := make([]ScoredMemo, 0, len(results))
	for _, r := range results {
		memoID, ok := parseMemoID(r.ID)
		if !ok {
			continue
		}
		out = append(out, ScoredMemo{MemoID: memoID, Similarity: r.Similarity})
	}
	return out, nil
}

// ContentSHA returns the last-indexed content SHA for a memo, or "" if the memo
// is not currently indexed. Used by the runner to skip unchanged memos.
func (s *Store) ContentSHA(memoID int32) string {
	if s == nil {
		return ""
	}
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	return s.index[memoID]
}

// Reconcile deletes vectors whose memo_id is not in validIDs.
// Returns the number of orphan documents removed.
//
// Use case: a memo was hard-deleted while the VectorStore was nil, or via a
// code path that bypassed the DeleteMemo service hook. Run periodically by the
// memoindex runner.
func (s *Store) Reconcile(ctx context.Context, validIDs map[int32]struct{}) (int, error) {
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
	for _, memoID := range indexed {
		if ctx.Err() != nil {
			return deleted, ctx.Err()
		}
		if _, exists := validIDs[memoID]; exists {
			continue
		}
		if err := s.collection.Delete(ctx, map[string]string{metaMemoID: strconv.Itoa(int(memoID))}, nil); err != nil {
			return deleted, errors.Wrapf(err, "delete orphan memo_id=%d", memoID)
		}
		s.setSHA(memoID, "")
		deleted++
	}
	return deleted, nil
}

// Model returns the embedding model this store is bound to.
func (s *Store) Model() string {
	if s == nil {
		return ""
	}
	return s.model
}

// setSHA updates the in-memory index and persists it. An empty sha removes the
// entry.
func (s *Store) setSHA(memoID int32, sha string) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()
	if sha == "" {
		delete(s.index, memoID)
	} else {
		s.index[memoID] = sha
	}
	if s.indexPath != "" {
		// Best-effort persistence; failures are logged via the returned error
		// path of the caller (mutations return their own errors). A sidecar
		// write failure does not corrupt the authoritative chromem-go data.
		_ = s.persistIndexLocked()
	}
}

func (s *Store) loadIndex() error {
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
		return errors.Wrap(err, "decode sidecar index")
	}
	if idx == nil {
		idx = make(map[int32]string)
	}
	s.index = idx
	return nil
}

func (s *Store) persistIndexLocked() error {
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

// Metadata key constants.
const (
	metaMemoID     = "memo_id"
	metaCreatorID  = "creator_id"
	metaVisibility = "visibility"
	metaRowStatus  = "row_status"
	metaContentSHA = "content_sha"
)

// docID is the chromem-go document ID for a memo.
func docID(id int32) string {
	return fmt.Sprintf("memo-%d", id)
}

// parseMemoID is the inverse of docID.
func parseMemoID(docID string) (int32, bool) {
	const prefix = "memo-"
	if !strings.HasPrefix(docID, prefix) {
		return 0, false
	}
	n, err := strconv.ParseInt(docID[len(prefix):], 10, 32)
	if err != nil || n <= 0 {
		return 0, false
	}
	return int32(n), true
}

// ComputeContentSHA returns the hex-encoded sha256 of the given plain text,
// truncated to the first 16 bytes for compactness. Used by callers that need
// to compute a content hash before upserting.
func ComputeContentSHA(plainText string) string {
	sum := sha256.Sum256([]byte(plainText))
	return hex.EncodeToString(sum[:16])
}

var (
	// fencedCodeBlock matches ``` ... ``` blocks (DOTALL-style via [^]).
	fencedCodeBlock = regexp.MustCompile("(?s)```.*?```")
	// inlineCode matches `code` and keeps the inner text.
	inlineCode = regexp.MustCompile("`([^`]*)`")
	// imageMarkdown matches ![alt](url).
	imageMarkdown = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	// linkMarkdown matches [text](url) and keeps the text.
	linkMarkdown = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// htmlTag matches remaining raw HTML tags.
	htmlTag = regexp.MustCompile(`<[^>]+>`)
)

// PlainText strips markdown noise (code fences, images, link URLs, raw HTML)
// while preserving the readable words. This is a minimal, dependency-free
// transform suitable as embedding input; it deliberately does not attempt full
// markdown rendering.
func PlainText(content string) string {
	s := fencedCodeBlock.ReplaceAllString(content, " ")
	s = inlineCode.ReplaceAllString(s, "$1")
	s = imageMarkdown.ReplaceAllString(s, " ")
	s = linkMarkdown.ReplaceAllString(s, "$1")
	s = htmlTag.ReplaceAllString(s, " ")
	return strings.Join(strings.Fields(s), " ")
}

// toPlainText is an alias kept for internal call sites.
func toPlainText(content string) string { return PlainText(content) }
