package test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/ai"
	embeddingsopenai "github.com/usememos/memos/internal/ai/embeddings/openai"
	"github.com/usememos/memos/internal/vector"
	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	apiv1 "github.com/usememos/memos/server/router/api/v1"
	"github.com/usememos/memos/store"
)

func injectAttachmentVectorStore(t *testing.T, ts *TestService, endpoint string) *vector.AttachmentStore {
	t.Helper()
	embedder, err := embeddingsopenai.New(ai.ProviderConfig{
		Type:     ai.ProviderOpenAI,
		Endpoint: endpoint,
		APIKey:   "sk-test",
	}, "nomic-embed-text")
	require.NoError(t, err)
	vs, err := vector.NewInMemoryAttachmentStore("nomic-embed-text", embedder)
	require.NoError(t, err)
	ts.Service.AttachmentVectorStore = vs
	return vs
}

func TestSearchAttachments_NotConfigured(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()

	user, err := ts.CreateRegularUser(ctx, "attachment-search-alice")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)

	_, err = ts.Service.SearchAttachments(userCtx, &v1pb.SearchAttachmentsRequest{Query: "invoice"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "FailedPrecondition")
}

func TestSearchAttachments_IndexRoundTrip(t *testing.T) {
	ctx := context.Background()
	ts := NewTestService(t)
	defer ts.Cleanup()
	mock := newMockEmbeddingsServer(t)
	defer mock.Close()
	vs := injectAttachmentVectorStore(t, ts, mock.URL)

	user, err := ts.CreateRegularUser(ctx, "attachment-search-bob")
	require.NoError(t, err)
	userCtx := ts.CreateUserContext(ctx, user.ID)

	attachment, err := ts.Service.CreateAttachment(userCtx, &v1pb.CreateAttachmentRequest{
		Attachment: &v1pb.Attachment{Filename: "receipt.png", Type: "image/png", Content: []byte("fake image")},
	})
	require.NoError(t, err)
	uid, err := apiv1.ExtractAttachmentUIDFromName(attachment.Name)
	require.NoError(t, err)
	stored, err := ts.Store.GetAttachment(ctx, &store.FindAttachment{UID: &uid})
	require.NoError(t, err)
	require.NotNil(t, stored)

	index := &store.AttachmentSearchIndex{
		AttachmentID:     stored.ID,
		ContentSHA:       "test-sha",
		OCRText:          "Cafe receipt total 42 dollars",
		Caption:          "A receipt from a cafe",
		TagsJSON:         `["receipt","cafe"]`,
		ObjectsJSON:      `["paper"]`,
		Status:           store.AttachmentSearchIndexStatusReady,
		VisionProviderID: "ollama",
		VisionModel:      "qwen3-vl:8b",
		EmbeddingModel:   "nomic-embed-text",
	}
	require.NoError(t, ts.Store.UpsertAttachmentSearchIndex(ctx, index))
	require.NoError(t, vs.UpsertAttachment(ctx, stored, index, index.ContentSHA))

	resp, err := ts.Service.SearchAttachments(userCtx, &v1pb.SearchAttachmentsRequest{Query: "coffee receipt", TopK: 5})
	require.NoError(t, err)
	require.Len(t, resp.GetResults(), 1)
	require.Equal(t, attachment.Name, resp.GetResults()[0].GetAttachment().GetName())
	require.NotEmpty(t, resp.GetResults()[0].GetSnippet())
}
