package v1

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/yaml.v3"

	"github.com/usememos/memos/server/auth"
	"github.com/usememos/memos/server/runner/memopayload"
	"github.com/usememos/memos/store"
)

const (
	markdownZipImportMaxFiles          = 10000
	markdownZipImportMaxUncompressed   = 512 << 20
	markdownZipImportMultipartMemory   = 32 << 20
	markdownZipImportDefaultVisibility = store.Private
	markdownZipImportDefaultMode       = "skip"
)

var (
	obsidianLinkRegexp = regexp.MustCompile(`!?\[\[([^\]|]+)(?:\|([^\]]+))?\]\]`)
	markdownImageRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
)

type markdownZipImportRequest struct {
	Visibility store.Visibility
	Mode       string
	DryRun     bool
}

type markdownZipImportReport struct {
	Created       int                          `json:"created"`
	Updated       int                          `json:"updated"`
	Skipped       int                          `json:"skipped"`
	Failed        int                          `json:"failed"`
	Attachments   int                          `json:"attachments"`
	MissingAssets []markdownZipImportIssue     `json:"missing_assets"`
	MissingLinks  []markdownZipImportIssue     `json:"missing_links"`
	Failures      []markdownZipImportIssue     `json:"failures"`
	Items         []markdownZipImportItemState `json:"items"`
}

type markdownZipImportIssue struct {
	File    string `json:"file"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

type markdownZipImportItemState struct {
	File   string `json:"file"`
	MemoID string `json:"memo_id"`
	Action string `json:"action"`
	Error  string `json:"error,omitempty"`
}

type markdownZipArchive struct {
	reader           *zip.ReadCloser
	userID           int32
	files            map[string]*zip.File
	markdownPaths    []string
	noteUIDByPath    map[string]string
	notePathsByTitle map[string][]string
	assetPathsByName map[string][]string
}

type markdownZipNote struct {
	Path      string
	Content   string
	CreatedTs *int64
	UpdatedTs *int64
}

type markdownZipAttachmentPlan struct {
	UID      string
	ZipPath  string
	Filename string
	URL      string
}

// ImportMarkdownZip imports an uploaded Obsidian-style Markdown zip archive.
func (s *APIV1Service) ImportMarkdownZip(c *echo.Context) error {
	ctx := c.Request().Context()
	authenticator := auth.NewAuthenticator(s.Store, s.Secret)
	result := authenticator.Authenticate(ctx, c.Request().Header.Get("Authorization"))
	if result == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	ctx = auth.ApplyToContext(ctx, result)
	user, err := s.fetchCurrentUser(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get current user")
	}
	if user == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "authentication required")
	}
	if !isSuperUser(user) {
		return echo.NewHTTPError(http.StatusForbidden, "admin permission required")
	}

	maxUploadBytes, err := s.importUploadSizeLimitBytes(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to get upload size limit")
	}
	c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, maxUploadBytes+1)
	if err := c.Request().ParseMultipartForm(markdownZipImportMultipartMemory); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "failed to parse multipart form")
	}
	importRequest, err := parseMarkdownZipImportRequest(c)
	if err != nil {
		return err
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "zip file is required")
	}
	if fileHeader.Size > maxUploadBytes {
		return echo.NewHTTPError(http.StatusBadRequest, "zip file exceeds the upload size limit")
	}
	if !strings.EqualFold(filepath.Ext(fileHeader.Filename), ".zip") {
		return echo.NewHTTPError(http.StatusBadRequest, "file must be a .zip archive")
	}

	zipPath, cleanup, err := saveUploadedZipToTemp(fileHeader)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to save uploaded zip")
	}
	defer cleanup()

	report, err := s.importMarkdownZip(ctx, user, zipPath, importRequest, maxUploadBytes)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, report)
}

func parseMarkdownZipImportRequest(c *echo.Context) (*markdownZipImportRequest, error) {
	visibility := markdownZipImportDefaultVisibility
	switch strings.ToUpper(strings.TrimSpace(c.FormValue("visibility"))) {
	case "", "PRIVATE":
		visibility = store.Private
	case "PUBLIC":
		visibility = store.Public
	case "PROTECTED":
		visibility = store.Protected
	default:
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid visibility")
	}

	mode := strings.ToLower(strings.TrimSpace(c.FormValue("mode")))
	if mode == "" {
		mode = markdownZipImportDefaultMode
	}
	if mode != "skip" && mode != "overwrite" {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid mode")
	}

	dryRun := false
	switch strings.ToLower(strings.TrimSpace(c.FormValue("dry_run"))) {
	case "", "false", "0", "no":
		dryRun = false
	case "true", "1", "yes":
		dryRun = true
	default:
		return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid dry_run value")
	}

	return &markdownZipImportRequest{Visibility: visibility, Mode: mode, DryRun: dryRun}, nil
}

func saveUploadedZipToTemp(fileHeader *multipart.FileHeader) (string, func(), error) {
	source, err := fileHeader.Open()
	if err != nil {
		return "", nil, err
	}
	defer source.Close()

	tempFile, err := os.CreateTemp("", "memos-markdown-import-*.zip")
	if err != nil {
		return "", nil, err
	}
	tempPath := tempFile.Name()
	cleanup := func() { _ = os.Remove(tempPath) }
	if _, err := io.Copy(tempFile, source); err != nil {
		_ = tempFile.Close()
		cleanup()
		return "", nil, err
	}
	if err := tempFile.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return tempPath, cleanup, nil
}

func (s *APIV1Service) importMarkdownZip(ctx context.Context, user *store.User, zipPath string, request *markdownZipImportRequest, uploadSizeLimit int64) (*markdownZipImportReport, error) {
	archive, err := readMarkdownZipArchive(zipPath, user.ID)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	defer archive.close()

	report := &markdownZipImportReport{
		MissingAssets: []markdownZipImportIssue{},
		MissingLinks:  []markdownZipImportIssue{},
		Failures:      []markdownZipImportIssue{},
		Items:         []markdownZipImportItemState{},
	}

	for _, notePath := range archive.markdownPaths {
		memoUID := archive.noteUIDByPath[notePath]
		item := markdownZipImportItemState{File: notePath, MemoID: memoUID}
		existing, err := s.Store.GetMemo(ctx, &store.FindMemo{UID: &memoUID})
		if err != nil {
			report.addFailure(notePath, "", "failed to check existing memo", err)
			item.Action = "failed"
			item.Error = err.Error()
			report.Items = append(report.Items, item)
			continue
		}
		if existing != nil && request.Mode == "skip" {
			report.Skipped++
			item.Action = "skipped"
			report.Items = append(report.Items, item)
			continue
		}

		note, err := parseMarkdownZipNote(archive.files[notePath])
		if err != nil {
			report.addFailure(notePath, "", "failed to read markdown file", err)
			item.Action = "failed"
			item.Error = err.Error()
			report.Items = append(report.Items, item)
			continue
		}
		note.Path = notePath

		content, attachments := transformMarkdownZipNote(archive, note, report, uploadSizeLimit)
		if request.DryRun {
			if existing == nil {
				report.Created++
				item.Action = "created"
			} else {
				report.Updated++
				item.Action = "updated"
			}
			report.Attachments += len(attachments)
			report.Items = append(report.Items, item)
			continue
		}

		memo, err := s.upsertImportedMemo(ctx, user, existing, memoUID, content, request.Visibility, note)
		if err != nil {
			report.addFailure(notePath, "", "failed to upsert memo", err)
			item.Action = "failed"
			item.Error = err.Error()
			report.Items = append(report.Items, item)
			continue
		}
		if existing == nil {
			report.Created++
			item.Action = "created"
		} else {
			report.Updated++
			item.Action = "updated"
		}

		for _, attachment := range attachments {
			if err := s.createOrLinkImportedAttachment(ctx, user, memo, archive.files[attachment.ZipPath], attachment); err != nil {
				report.addFailure(notePath, attachment.ZipPath, "failed to import attachment", err)
				continue
			}
			report.Attachments++
		}
		report.Items = append(report.Items, item)
	}

	return report, nil
}

func readMarkdownZipArchive(zipPath string, userID int32) (*markdownZipArchive, error) {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("invalid zip archive")
	}
	success := false
	defer func() {
		if !success {
			_ = reader.Close()
		}
	}()

	archive := &markdownZipArchive{
		reader:           reader,
		userID:           userID,
		files:            map[string]*zip.File{},
		noteUIDByPath:    map[string]string{},
		notePathsByTitle: map[string][]string{},
		assetPathsByName: map[string][]string{},
	}

	var totalUncompressed uint64
	for _, file := range reader.File {
		if len(archive.files) >= markdownZipImportMaxFiles {
			return nil, fmt.Errorf("zip archive contains too many files")
		}
		cleanPath, ok := cleanZipEntryPath(file.Name)
		if !ok {
			return nil, fmt.Errorf("zip archive contains unsafe path %q", file.Name)
		}
		if file.FileInfo().IsDir() || shouldSkipMarkdownZipPath(cleanPath) {
			continue
		}
		totalUncompressed += file.UncompressedSize64
		if totalUncompressed > markdownZipImportMaxUncompressed {
			return nil, fmt.Errorf("zip archive exceeds the uncompressed size limit")
		}
		archive.files[cleanPath] = file
	}

	for filePath := range archive.files {
		if strings.EqualFold(path.Ext(filePath), ".md") {
			archive.markdownPaths = append(archive.markdownPaths, filePath)
			uid := stableImportUID("obs", stableImportSeed(userID, filePath), 32)
			archive.noteUIDByPath[filePath] = uid
			title := strings.ToLower(strings.TrimSuffix(path.Base(filePath), path.Ext(filePath)))
			archive.notePathsByTitle[title] = append(archive.notePathsByTitle[title], filePath)
			archive.notePathsByTitle[strings.ToLower(strings.TrimSuffix(filePath, path.Ext(filePath)))] = append(archive.notePathsByTitle[strings.ToLower(strings.TrimSuffix(filePath, path.Ext(filePath)))], filePath)
		} else {
			archive.assetPathsByName[strings.ToLower(path.Base(filePath))] = append(archive.assetPathsByName[strings.ToLower(path.Base(filePath))], filePath)
		}
	}
	sort.Strings(archive.markdownPaths)
	success = true
	return archive, nil
}

func (archive *markdownZipArchive) close() {
	if archive.reader != nil {
		_ = archive.reader.Close()
	}
}

func cleanZipEntryPath(name string) (string, bool) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", false
	}
	cleaned := path.Clean(name)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return "", false
	}
	return cleaned, true
}

func shouldSkipMarkdownZipPath(filePath string) bool {
	parts := strings.Split(filePath, "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == ".obsidian" || strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}

func parseMarkdownZipNote(file *zip.File) (*markdownZipNote, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	content, frontmatter := splitMarkdownFrontmatter(string(body))
	note := &markdownZipNote{Path: file.Name, Content: content}
	if len(frontmatter) == 0 {
		return note, nil
	}

	var metadata map[string]any
	if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
		return note, nil
	}
	if created := parseMarkdownZipTime(metadata["created"]); created != nil {
		note.CreatedTs = created
	}
	if updated := parseMarkdownZipTime(metadata["updated"]); updated != nil {
		note.UpdatedTs = updated
	} else if modified := parseMarkdownZipTime(metadata["modified"]); modified != nil {
		note.UpdatedTs = modified
	}
	if tags := normalizeMarkdownZipTags(metadata["tags"]); len(tags) > 0 {
		note.Content = appendMarkdownZipTags(note.Content, tags)
	}
	return note, nil
}

func splitMarkdownFrontmatter(content string) (string, string) {
	trimmed := strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
		return content, ""
	}
	lineEnding := "\n"
	if strings.HasPrefix(trimmed, "---\r\n") {
		lineEnding = "\r\n"
	}
	start := len("---" + lineEnding)
	marker := lineEnding + "---" + lineEnding
	idx := strings.Index(trimmed[start:], marker)
	if idx < 0 {
		return content, ""
	}
	frontmatter := trimmed[start : start+idx]
	body := trimmed[start+idx+len(marker):]
	return body, frontmatter
}

func parseMarkdownZipTime(value any) *int64 {
	switch v := value.(type) {
	case time.Time:
		unix := v.Unix()
		return &unix
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil
		}
		layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}
		for _, layout := range layouts {
			if t, err := time.Parse(layout, text); err == nil {
				unix := t.Unix()
				return &unix
			}
		}
	}
	return nil
}

func normalizeMarkdownZipTags(value any) []string {
	var tags []string
	add := func(tag string) {
		tag = strings.TrimSpace(strings.TrimPrefix(tag, "#"))
		if tag == "" || strings.ContainsAny(tag, " \t\r\n") {
			return
		}
		tags = append(tags, tag)
	}
	switch v := value.(type) {
	case string:
		for _, tag := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
			add(tag)
		}
	case []any:
		for _, item := range v {
			if tag, ok := item.(string); ok {
				add(tag)
			}
		}
	case []string:
		for _, tag := range v {
			add(tag)
		}
	}
	return tags
}

func appendMarkdownZipTags(content string, tags []string) string {
	if len(tags) == 0 {
		return content
	}
	formatted := make([]string, 0, len(tags))
	for _, tag := range tags {
		formatted = append(formatted, "#"+tag)
	}
	return strings.TrimRight(content, "\r\n") + "\n\n" + strings.Join(formatted, " ") + "\n"
}

func transformMarkdownZipNote(archive *markdownZipArchive, note *markdownZipNote, report *markdownZipImportReport, uploadSizeLimit int64) (string, []markdownZipAttachmentPlan) {
	attachmentsByUID := map[string]markdownZipAttachmentPlan{}
	content := obsidianLinkRegexp.ReplaceAllStringFunc(note.Content, func(match string) string {
		submatches := obsidianLinkRegexp.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		embedded := strings.HasPrefix(match, "!")
		target := strings.TrimSpace(submatches[1])
		alias := ""
		if len(submatches) > 2 {
			alias = strings.TrimSpace(submatches[2])
		}
		if embedded {
			if assetPath, ok := archive.resolveAssetPath(note.Path, target); ok {
				plan, ok := archive.buildAttachmentPlan(note.Path, assetPath, uploadSizeLimit)
				if !ok {
					report.MissingAssets = append(report.MissingAssets, markdownZipImportIssue{File: note.Path, Target: target, Message: "asset exceeds upload size limit"})
					return match
				}
				attachmentsByUID[plan.UID] = plan
				alt := path.Base(assetPath)
				return fmt.Sprintf("![%s](%s)", alt, plan.URL)
			}
			report.MissingAssets = append(report.MissingAssets, markdownZipImportIssue{File: note.Path, Target: target, Message: "asset not found"})
			return match
		}

		if memoUID, ok := archive.resolveNoteUID(note.Path, target); ok {
			label := alias
			if label == "" {
				label = strings.TrimSpace(strings.Split(target, "#")[0])
			}
			return fmt.Sprintf("[%s](/memos/%s)", label, memoUID)
		}
		report.MissingLinks = append(report.MissingLinks, markdownZipImportIssue{File: note.Path, Target: target, Message: "note not found"})
		return match
	})

	content = markdownImageRegex.ReplaceAllStringFunc(content, func(match string) string {
		submatches := markdownImageRegex.FindStringSubmatch(match)
		if len(submatches) < 3 {
			return match
		}
		target := strings.TrimSpace(strings.Trim(submatches[2], "\"'"))
		if isExternalMarkdownZipURL(target) {
			return match
		}
		assetPath, ok := archive.resolveAssetPath(note.Path, target)
		if !ok {
			report.MissingAssets = append(report.MissingAssets, markdownZipImportIssue{File: note.Path, Target: target, Message: "asset not found"})
			return match
		}
		plan, ok := archive.buildAttachmentPlan(note.Path, assetPath, uploadSizeLimit)
		if !ok {
			report.MissingAssets = append(report.MissingAssets, markdownZipImportIssue{File: note.Path, Target: target, Message: "asset exceeds upload size limit"})
			return match
		}
		attachmentsByUID[plan.UID] = plan
		return fmt.Sprintf("![%s](%s)", submatches[1], plan.URL)
	})

	attachments := make([]markdownZipAttachmentPlan, 0, len(attachmentsByUID))
	for _, attachment := range attachmentsByUID {
		attachments = append(attachments, attachment)
	}
	sort.Slice(attachments, func(i, j int) bool { return attachments[i].UID < attachments[j].UID })
	return content, attachments
}

func (archive *markdownZipArchive) resolveNoteUID(currentNotePath, target string) (string, bool) {
	target = strings.TrimSpace(strings.Split(target, "#")[0])
	if target == "" {
		return "", false
	}
	candidates := []string{}
	if strings.EqualFold(path.Ext(target), ".md") {
		candidates = append(candidates, path.Clean(path.Join(path.Dir(currentNotePath), target)))
	} else {
		candidates = append(candidates, path.Clean(path.Join(path.Dir(currentNotePath), target+".md")))
	}
	for _, candidate := range candidates {
		if uid, ok := archive.noteUIDByPath[candidate]; ok {
			return uid, true
		}
	}

	lookup := strings.ToLower(strings.TrimSuffix(target, path.Ext(target)))
	paths := archive.notePathsByTitle[lookup]
	if len(paths) == 1 {
		return archive.noteUIDByPath[paths[0]], true
	}
	return "", false
}

func (archive *markdownZipArchive) resolveAssetPath(currentNotePath, target string) (string, bool) {
	target = strings.TrimSpace(strings.Split(target, "#")[0])
	if target == "" || isExternalMarkdownZipURL(target) || strings.HasPrefix(target, "/") {
		return "", false
	}
	if strings.Contains(target, "|") {
		target = strings.TrimSpace(strings.Split(target, "|")[0])
	}
	relative := path.Clean(path.Join(path.Dir(currentNotePath), target))
	if file, ok := archive.files[relative]; ok && !strings.EqualFold(path.Ext(file.Name), ".md") {
		return relative, true
	}
	paths := archive.assetPathsByName[strings.ToLower(path.Base(target))]
	if len(paths) == 1 {
		return paths[0], true
	}
	return "", false
}

func (archive *markdownZipArchive) buildAttachmentPlan(notePath, assetPath string, uploadSizeLimit int64) (markdownZipAttachmentPlan, bool) {
	file := archive.files[assetPath]
	if file == nil || int64(file.UncompressedSize64) > uploadSizeLimit {
		return markdownZipAttachmentPlan{}, false
	}
	filename := path.Base(assetPath)
	uid := stableImportUID("obsatt", stableImportSeed(archive.userID, notePath+"\x00"+assetPath), 29)
	return markdownZipAttachmentPlan{
		UID:      uid,
		ZipPath:  assetPath,
		Filename: filename,
		URL:      fmt.Sprintf("/file/attachments/%s/%s", uid, url.PathEscape(filename)),
	}, true
}

func isExternalMarkdownZipURL(rawURL string) bool {
	if strings.HasPrefix(rawURL, "/file/attachments/") {
		return true
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https" || parsed.Scheme == "data" || strings.HasPrefix(rawURL, "#")
}

func (s *APIV1Service) upsertImportedMemo(ctx context.Context, user *store.User, existing *store.Memo, uid, content string, visibility store.Visibility, note *markdownZipNote) (*store.Memo, error) {
	memo := &store.Memo{
		UID:        uid,
		CreatorID:  user.ID,
		Content:    content,
		Visibility: visibility,
	}
	if note.CreatedTs != nil {
		memo.CreatedTs = *note.CreatedTs
	}
	if note.UpdatedTs != nil {
		memo.UpdatedTs = *note.UpdatedTs
	}
	if err := memopayload.RebuildMemoPayload(ctx, memo, s.MarkdownService); err != nil {
		return nil, err
	}

	if existing == nil {
		created, err := s.Store.CreateMemo(ctx, memo)
		if err != nil {
			return nil, err
		}
		s.indexMemoAsync(created)
		return created, nil
	}
	if !canModifyMemo(user, existing) {
		return nil, status.Errorf(codes.PermissionDenied, "permission denied")
	}
	existing.Content = content
	existing.Visibility = visibility
	existing.Payload = memo.Payload
	update := &store.UpdateMemo{
		ID:         existing.ID,
		Content:    &content,
		Visibility: &visibility,
		Payload:    memo.Payload,
	}
	if note.CreatedTs != nil {
		update.CreatedTs = note.CreatedTs
	}
	if note.UpdatedTs != nil {
		update.UpdatedTs = note.UpdatedTs
	} else {
		now := time.Now().Unix()
		update.UpdatedTs = &now
	}
	if err := s.Store.UpdateMemo(ctx, update); err != nil {
		return nil, err
	}
	s.indexMemoAsync(existing)
	return existing, nil
}

func (s *APIV1Service) createOrLinkImportedAttachment(ctx context.Context, user *store.User, memo *store.Memo, file *zip.File, plan markdownZipAttachmentPlan) error {
	existing, err := s.Store.GetAttachment(ctx, &store.FindAttachment{UID: &plan.UID})
	if err != nil {
		return err
	}
	if existing != nil {
		if existing.CreatorID != user.ID && !isSuperUser(user) {
			return status.Errorf(codes.PermissionDenied, "cannot reuse another user's attachment")
		}
		if existing.MemoID == nil || *existing.MemoID != memo.ID {
			updatedTs := time.Now().Unix()
			return s.Store.UpdateAttachment(ctx, &store.UpdateAttachment{ID: existing.ID, MemoID: &memo.ID, UpdatedTs: &updatedTs})
		}
		return nil
	}

	if !validateFilename(plan.Filename) {
		return fmt.Errorf("invalid attachment filename %q", plan.Filename)
	}
	reader, err := file.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	blob, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	mimeType := mime.TypeByExtension(path.Ext(plan.Filename))
	if mimeType == "" {
		mimeType = http.DetectContentType(blob)
	}
	normalizedType, ok := normalizeMimeType(mimeType)
	if !ok {
		normalizedType = "application/octet-stream"
	}
	create := &store.Attachment{
		UID:       plan.UID,
		CreatorID: user.ID,
		Filename:  plan.Filename,
		Type:      normalizedType,
		Size:      int64(len(blob)),
		Blob:      blob,
		MemoID:    &memo.ID,
	}
	if shouldStripExif(create.Type) {
		release, err := s.acquireImageProcessingSlot(ctx)
		if err != nil {
			return status.Errorf(codes.ResourceExhausted, "too many image processing requests")
		}
		strippedBlob, stripErr := stripImageExif(create.Blob, create.Type)
		release()
		if stripErr != nil {
			slog.Warn("failed to strip EXIF metadata from imported image", "type", create.Type, "filename", create.Filename, "error", stripErr)
		} else {
			create.Blob = strippedBlob
			create.Size = int64(len(strippedBlob))
		}
	}
	if err := SaveAttachmentBlob(ctx, s.Profile, s.Store, create); err != nil {
		return err
	}
	_, err = s.Store.CreateAttachment(ctx, create)
	return err
}

func (s *APIV1Service) importUploadSizeLimitBytes(ctx context.Context) (int64, error) {
	setting, err := s.Store.GetInstanceStorageSetting(ctx)
	if err != nil {
		return 0, err
	}
	limit := setting.UploadSizeLimitMb * MebiByte
	if limit == 0 {
		limit = MaxUploadBufferSizeBytes
	}
	return limit, nil
}

func stableImportUID(prefix, value string, hashChars int) string {
	sum := sha256.Sum256([]byte(value))
	hash := hex.EncodeToString(sum[:])
	if hashChars > len(hash) {
		hashChars = len(hash)
	}
	return prefix + "-" + hash[:hashChars]
}

func stableImportSeed(userID int32, value string) string {
	return fmt.Sprintf("user:%d:%s", userID, value)
}

func (r *markdownZipImportReport) addFailure(file, target, message string, err error) {
	r.Failed++
	issue := markdownZipImportIssue{File: file, Target: target, Message: message}
	if err != nil {
		issue.Message = message + ": " + err.Error()
	}
	r.Failures = append(r.Failures, issue)
}
