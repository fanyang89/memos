package v1

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/store"
	teststore "github.com/usememos/memos/store/test"
)

func TestImportMarkdownZipCreatesPrivateMemosAndAttachments(t *testing.T) {
	ctx := context.Background()
	svc, user, cleanup := newMarkdownZipImportTestService(ctx, t)
	defer cleanup()

	zipPath := writeMarkdownZipImportTestArchive(t, map[string][]byte{
		"Vault/A.md":    []byte("---\ntags:\n  - imported\ncreated: 2024-01-02\n---\n# A\nSee [[B|Bee]].\n![[pic.png]]\n"),
		"Vault/B.md":    []byte("# B\n"),
		"Vault/pic.png": []byte{0x89, 0x50, 0x4e, 0x47},
	})

	report, err := svc.importMarkdownZip(ctx, user, zipPath, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 2, report.Created)
	require.Equal(t, 1, report.Attachments)
	require.Empty(t, report.Failures)
	require.Empty(t, report.MissingAssets)

	memoAUID := stableImportUID("obs", stableImportSeed(user.ID, "Vault/A.md"), 32)
	memoBUID := stableImportUID("obs", stableImportSeed(user.ID, "Vault/B.md"), 32)
	memoA, err := svc.Store.GetMemo(ctx, &store.FindMemo{UID: &memoAUID})
	require.NoError(t, err)
	require.NotNil(t, memoA)
	require.Equal(t, store.Private, memoA.Visibility)
	require.Contains(t, memoA.Content, "[Bee](/memos/"+memoBUID+")")
	require.Contains(t, memoA.Content, "#imported")
	require.Contains(t, memoA.Content, "/file/attachments/")
	require.Equal(t, time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).Unix(), memoA.CreatedTs)

	attachments, err := svc.Store.ListAttachments(ctx, &store.FindAttachment{MemoID: &memoA.ID})
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Equal(t, "pic.png", attachments[0].Filename)
}

func TestImportMarkdownZipUIDsAreScopedByUser(t *testing.T) {
	ctx := context.Background()
	svc, firstUser, cleanup := newMarkdownZipImportTestService(ctx, t)
	defer cleanup()
	secondUser, err := svc.Store.CreateUser(ctx, &store.User{
		Username: "second-importer",
		Role:     store.RoleUser,
		Email:    "second-importer@example.com",
	})
	require.NoError(t, err)

	zipPath := writeMarkdownZipImportTestArchive(t, map[string][]byte{"Note.md": []byte("same path")})
	firstReport, err := svc.importMarkdownZip(ctx, firstUser, zipPath, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, firstReport.Created)
	secondReport, err := svc.importMarkdownZip(ctx, secondUser, zipPath, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, secondReport.Created)
	require.Equal(t, 0, secondReport.Skipped)

	firstUID := stableImportUID("obs", stableImportSeed(firstUser.ID, "Note.md"), 32)
	secondUID := stableImportUID("obs", stableImportSeed(secondUser.ID, "Note.md"), 32)
	require.NotEqual(t, firstUID, secondUID)
}

func TestImportMarkdownZipSkipAndOverwrite(t *testing.T) {
	ctx := context.Background()
	svc, user, cleanup := newMarkdownZipImportTestService(ctx, t)
	defer cleanup()

	firstZip := writeMarkdownZipImportTestArchive(t, map[string][]byte{"Note.md": []byte("original")})
	secondZip := writeMarkdownZipImportTestArchive(t, map[string][]byte{"Note.md": []byte("changed")})

	report, err := svc.importMarkdownZip(ctx, user, firstZip, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)

	report, err = svc.importMarkdownZip(ctx, user, secondZip, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, report.Skipped)
	memoUID := stableImportUID("obs", stableImportSeed(user.ID, "Note.md"), 32)
	memo, err := svc.Store.GetMemo(ctx, &store.FindMemo{UID: &memoUID})
	require.NoError(t, err)
	require.Equal(t, "original", memo.Content)

	report, err = svc.importMarkdownZip(ctx, user, secondZip, &markdownZipImportRequest{Visibility: store.Private, Mode: "overwrite"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, report.Updated)
	memo, err = svc.Store.GetMemo(ctx, &store.FindMemo{UID: &memoUID})
	require.NoError(t, err)
	require.Equal(t, "changed", memo.Content)
}

func TestImportMarkdownZipAllowsLongMemoContent(t *testing.T) {
	ctx := context.Background()
	svc, user, cleanup := newMarkdownZipImportTestService(ctx, t)
	defer cleanup()

	longContent := strings.Repeat("x", store.DefaultContentLengthLimit+1)
	zipPath := writeMarkdownZipImportTestArchive(t, map[string][]byte{"Long.md": []byte(longContent)})
	report, err := svc.importMarkdownZip(ctx, user, zipPath, &markdownZipImportRequest{Visibility: store.Private, Mode: "skip"}, MaxUploadBufferSizeBytes)
	require.NoError(t, err)
	require.Equal(t, 1, report.Created)

	memoUID := stableImportUID("obs", stableImportSeed(user.ID, "Long.md"), 32)
	memo, err := svc.Store.GetMemo(ctx, &store.FindMemo{UID: &memoUID})
	require.NoError(t, err)
	require.Equal(t, longContent, memo.Content)
}

func newMarkdownZipImportTestService(ctx context.Context, t *testing.T) (*APIV1Service, *store.User, func()) {
	t.Helper()
	storeInstance := teststore.NewTestingStore(ctx, t)
	svc := NewAPIV1Service("test-secret", &profile.Profile{
		Demo:   true,
		Driver: "sqlite",
		DSN:    ":memory:",
		Data:   storeInstance.GetDataDir(),
	}, storeInstance)
	user, err := storeInstance.CreateUser(ctx, &store.User{
		Username: "importer",
		Role:     store.RoleUser,
		Email:    "importer@example.com",
	})
	require.NoError(t, err)
	return svc, user, func() { storeInstance.Close() }
}

func writeMarkdownZipImportTestArchive(t *testing.T, files map[string][]byte) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "vault.zip")
	file, err := os.Create(zipPath)
	require.NoError(t, err)
	writer := zip.NewWriter(file)
	for name, content := range files {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = entry.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	require.NoError(t, file.Close())
	return zipPath
}
