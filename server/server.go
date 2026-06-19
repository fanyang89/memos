package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai"
	embeddingsopenai "github.com/usememos/memos/internal/ai/embeddings/openai"
	"github.com/usememos/memos/internal/ai/vision"
	visionopenai "github.com/usememos/memos/internal/ai/vision/openai"
	"github.com/usememos/memos/internal/profile"
	"github.com/usememos/memos/internal/vector"
	storepb "github.com/usememos/memos/proto/gen/store"
	apiv1 "github.com/usememos/memos/server/router/api/v1"
	"github.com/usememos/memos/server/router/fileserver"
	"github.com/usememos/memos/server/router/frontend"
	"github.com/usememos/memos/server/router/mcp"
	"github.com/usememos/memos/server/router/rss"
	"github.com/usememos/memos/server/runner/attachmentindex"
	"github.com/usememos/memos/server/runner/memoindex"
	"github.com/usememos/memos/server/runner/s3presign"
	"github.com/usememos/memos/store"
)

const shutdownTimeout = 10 * time.Second

type Server struct {
	Secret  string
	Profile *profile.Profile
	Store   *store.Store

	// VectorStore backs semantic search. nil when embedding is unconfigured or
	// the persistent vector DB failed to open; in that case the memoindex
	// runner is skipped and SearchMemos returns FailedPrecondition.
	VectorStore *vector.Store
	// AttachmentVectorStore backs attachment image semantic search.
	AttachmentVectorStore *vector.AttachmentStore
	ImageAnalyzer         vision.Analyzer
	imageVisionProviderID string
	imageVisionModel      string
	imageSearchPrompt     string

	echoServer *echo.Echo
	httpServer *http.Server
	sseHub     *apiv1.SSEHub
	apiV1      *apiv1.APIV1Service

	backgroundRunnerCancels []context.CancelFunc
	backgroundRunnerWG      sync.WaitGroup
}

func NewServer(ctx context.Context, profile *profile.Profile, store *store.Store) (*Server, error) {
	s := &Server{
		Store:   store,
		Profile: profile,
	}

	echoServer := echo.New()
	echoServer.Use(middleware.Recover())
	echoServer.Use(newCORSMiddleware(profile))
	s.echoServer = echoServer

	instanceBasicSetting, err := s.getOrUpsertInstanceBasicSetting(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get instance basic setting")
	}

	secret := "usememos"
	if !profile.Demo {
		secret = instanceBasicSetting.SecretKey
	}
	s.Secret = secret

	// Register healthz endpoint.
	echoServer.GET("/healthz", func(c *echo.Context) error {
		return c.String(http.StatusOK, "Service ready.")
	})

	// Serve frontend static files.
	frontend.NewFrontendService(profile, store).Serve(ctx, echoServer)

	rootGroup := echoServer.Group("")

	apiV1Service := apiv1.NewAPIV1Service(s.Secret, profile, store)
	s.apiV1 = apiV1Service
	s.sseHub = apiV1Service.SSEHub

	// Initialize the semantic-search vector store from the persisted AI
	// embedding config. Failures are non-fatal: SearchMemos and the memoindex
	// runner degrade to disabled when VectorStore stays nil.
	s.VectorStore = s.initVectorStore(ctx)
	apiV1Service.VectorStore = s.VectorStore
	s.AttachmentVectorStore, s.ImageAnalyzer, s.imageVisionProviderID, s.imageVisionModel, s.imageSearchPrompt = s.initAttachmentSearch(ctx)
	apiV1Service.AttachmentVectorStore = s.AttachmentVectorStore

	// Register HTTP file server routes BEFORE gRPC-Gateway to ensure proper range request handling for Safari.
	// This uses native HTTP serving (http.ServeContent) instead of gRPC for video/audio files.
	fileServerService := fileserver.NewFileServerService(s.Profile, s.Store, s.Secret)
	fileServerService.RegisterRoutes(echoServer)

	// Create and register RSS routes (needs markdown service from apiV1Service).
	rss.NewRSSService(s.Profile, s.Store, apiV1Service.MarkdownService).RegisterRoutes(rootGroup)

	// Register gRPC gateway as api v1 (includes SSE endpoint on CORS-enabled group).
	if err := apiV1Service.RegisterGateway(ctx, echoServer); err != nil {
		return nil, errors.Wrap(err, "failed to register gRPC gateway")
	}

	mcpService, err := mcp.NewMCPService(profile, echoServer)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create MCP service")
	}
	mcpService.RegisterRoutes(echoServer)

	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	var address, network string
	if len(s.Profile.UNIXSock) == 0 {
		address = fmt.Sprintf("%s:%d", s.Profile.Addr, s.Profile.Port)
		network = "tcp"
	} else {
		address = s.Profile.UNIXSock
		network = "unix"
	}
	listener, err := net.Listen(network, address)
	if err != nil {
		return errors.Wrap(err, "failed to listen")
	}

	if network == "unix" {
		if err := os.Chmod(address, 0660); err != nil {
			_ = listener.Close()
			return errors.Wrap(err, "failed to chmod socket")
		}
	}

	// Start Echo server directly (no cmux needed - all traffic is HTTP).
	s.httpServer = &http.Server{Handler: s.echoServer}
	go func() {
		if err := s.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			slog.Error("failed to start echo server", "error", err)
		}
	}()
	s.startBackgroundRunners(ctx)

	return nil
}

func (s *Server) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()

	slog.Info("server shutting down")

	s.stopBackgroundRunners()
	s.closeLongLivedConnections()
	s.shutdownHTTPServer(ctx)
	s.waitBackgroundRunners(ctx)

	// Close database connection.
	if err := s.Store.Close(); err != nil {
		slog.Error("failed to close database", slog.String("error", err.Error()))
	}

	slog.Info("memos stopped properly")
}

func (s *Server) startBackgroundRunners(ctx context.Context) {
	// Create a separate context for each background runner
	// This allows us to control cancellation for each runner independently
	s3Context, s3Cancel := context.WithCancel(ctx)

	// Store the cancel function so we can properly shut down runners
	s.backgroundRunnerCancels = append(s.backgroundRunnerCancels, s3Cancel)

	// Create and start S3 presign runner
	s3presignRunner := s3presign.NewRunner(s.Store)
	s3presignRunner.RunOnce(ctx)

	// Start continuous S3 presign runner
	s.backgroundRunnerWG.Add(1)
	go func() {
		defer s.backgroundRunnerWG.Done()
		s3presignRunner.Run(s3Context)
		slog.Info("s3presign runner stopped")
	}()

	// Semantic index runner: only when the vector store initialized (embedding configured).
	if s.VectorStore != nil {
		indexCtx, indexCancel := context.WithCancel(ctx)
		s.backgroundRunnerCancels = append(s.backgroundRunnerCancels, indexCancel)

		memoindexRunner := memoindex.NewRunner(s.Store, s.VectorStore, s.Profile.MemoIndexInterval)
		// Synchronous backfill on startup so newly-configured embeddings catch up
		// before the first tick.
		memoindexRunner.RunOnce(ctx)

		s.backgroundRunnerWG.Add(1)
		go func() {
			defer s.backgroundRunnerWG.Done()
			memoindexRunner.Run(indexCtx)
			slog.Info("memoindex runner stopped")
		}()
		slog.Info("memoindex runner started", "interval", s.Profile.MemoIndexInterval.String())
	} else {
		slog.Info("memoindex disabled: embedding not configured or vector store init failed")
	}

	if s.AttachmentVectorStore != nil && s.ImageAnalyzer != nil && s.apiV1 != nil {
		indexCtx, indexCancel := context.WithCancel(ctx)
		s.backgroundRunnerCancels = append(s.backgroundRunnerCancels, indexCancel)

		attachmentindexRunner := attachmentindex.NewRunner(
			s.Store,
			s.AttachmentVectorStore,
			s.ImageAnalyzer,
			s.apiV1,
			s.Profile.MemoIndexInterval,
			s.imageVisionProviderID,
			s.imageVisionModel,
			s.imageSearchPrompt,
		)
		attachmentindexRunner.RunOnce(ctx)

		s.backgroundRunnerWG.Add(1)
		go func() {
			defer s.backgroundRunnerWG.Done()
			attachmentindexRunner.Run(indexCtx)
			slog.Info("attachmentindex runner stopped")
		}()
		slog.Info("attachmentindex runner started", "interval", s.Profile.MemoIndexInterval.String(), "vision_model", s.imageVisionModel)
	} else {
		slog.Info("attachmentindex disabled: image search or embedding not configured")
	}

	slog.Info("background runners started")
}

// initVectorStore builds the semantic-search vector store from the persisted AI
// embedding configuration. Returns nil (semantic search disabled) when no
// embedding provider is configured, the provider is unsupported, or the
// persistent DB cannot be opened.
func (s *Server) initVectorStore(ctx context.Context) *vector.Store {
	setting, err := s.Store.GetInstanceAISetting(ctx)
	if err != nil {
		slog.Error("failed to get AI setting for vector store init", "error", err)
		return nil
	}
	embCfg := setting.GetEmbedding()
	if embCfg.GetProviderId() == "" {
		return nil
	}

	providers := make([]ai.ProviderConfig, 0, len(setting.GetProviders()))
	for _, provider := range setting.GetProviders() {
		if provider == nil {
			continue
		}
		providers = append(providers, ai.ProviderConfig{
			ID:       provider.GetId(),
			Title:    provider.GetTitle(),
			Type:     convertAIProviderTypeFromStore(provider.GetType()),
			Endpoint: provider.GetEndpoint(),
			APIKey:   provider.GetApiKey(),
		})
	}
	provider, err := ai.FindProvider(providers, embCfg.GetProviderId())
	if err != nil {
		slog.Warn("embedding provider not found, semantic search disabled", "provider_id", embCfg.GetProviderId())
		return nil
	}
	if provider.Type != ai.ProviderOpenAI {
		slog.Warn("embedding provider type unsupported, semantic search disabled", "type", provider.Type)
		return nil
	}

	model := embCfg.GetModel()
	if model == "" {
		model, err = ai.DefaultEmbeddingModel(provider.Type)
		if err != nil {
			slog.Warn("no embedding model resolved, semantic search disabled", "error", err)
			return nil
		}
	}

	embedder, err := embeddingsopenai.New(*provider, model)
	if err != nil {
		slog.Error("failed to construct embedding client, semantic search disabled", "error", err)
		return nil
	}
	vstore, err := vector.NewPersistent(ctx, s.Profile.Data, model, embedder)
	if err != nil {
		slog.Error("failed to open vector store, semantic search disabled", "error", err)
		return nil
	}
	slog.Info("vector store initialized", "model", model, "provider", provider.ID)
	return vstore
}

// initAttachmentSearch builds the attachment-search vector store and image analyzer.
func (s *Server) initAttachmentSearch(ctx context.Context) (*vector.AttachmentStore, vision.Analyzer, string, string, string) {
	setting, err := s.Store.GetInstanceAISetting(ctx)
	if err != nil {
		slog.Error("failed to get AI setting for attachment search init", "error", err)
		return nil, nil, "", "", ""
	}
	imageCfg := setting.GetImageSearch()
	if imageCfg.GetProviderId() == "" {
		return nil, nil, "", "", ""
	}
	embCfg := setting.GetEmbedding()
	if embCfg.GetProviderId() == "" {
		slog.Warn("attachment search disabled: embedding provider is required")
		return nil, nil, "", "", ""
	}

	providers := make([]ai.ProviderConfig, 0, len(setting.GetProviders()))
	for _, provider := range setting.GetProviders() {
		if provider == nil {
			continue
		}
		providers = append(providers, ai.ProviderConfig{
			ID:       provider.GetId(),
			Title:    provider.GetTitle(),
			Type:     convertAIProviderTypeFromStore(provider.GetType()),
			Endpoint: provider.GetEndpoint(),
			APIKey:   provider.GetApiKey(),
		})
	}
	embeddingProvider, err := ai.FindProvider(providers, embCfg.GetProviderId())
	if err != nil {
		slog.Warn("attachment search embedding provider not found", "provider_id", embCfg.GetProviderId())
		return nil, nil, "", "", ""
	}
	if embeddingProvider.Type != ai.ProviderOpenAI {
		slog.Warn("attachment search embedding provider type unsupported", "type", embeddingProvider.Type)
		return nil, nil, "", "", ""
	}
	visionProvider, err := ai.FindProvider(providers, imageCfg.GetProviderId())
	if err != nil {
		slog.Warn("attachment search vision provider not found", "provider_id", imageCfg.GetProviderId())
		return nil, nil, "", "", ""
	}
	if visionProvider.Type != ai.ProviderOpenAI {
		slog.Warn("attachment search vision provider type unsupported", "type", visionProvider.Type)
		return nil, nil, "", "", ""
	}

	embeddingModel := embCfg.GetModel()
	if embeddingModel == "" {
		embeddingModel, err = ai.DefaultEmbeddingModel(embeddingProvider.Type)
		if err != nil {
			slog.Warn("attachment search embedding model unresolved", "error", err)
			return nil, nil, "", "", ""
		}
	}
	visionModel := imageCfg.GetVisionModel()
	if visionModel == "" {
		visionModel, err = ai.DefaultVisionModel(visionProvider.Type)
		if err != nil {
			slog.Warn("attachment search vision model unresolved", "error", err)
			return nil, nil, "", "", ""
		}
	}

	embedder, err := embeddingsopenai.New(*embeddingProvider, embeddingModel)
	if err != nil {
		slog.Error("failed to construct attachment embedding client", "error", err)
		return nil, nil, "", "", ""
	}
	attachmentVectorStore, err := vector.NewPersistentAttachmentStore(ctx, s.Profile.Data, embeddingModel, embedder)
	if err != nil {
		slog.Error("failed to open attachment vector store", "error", err)
		return nil, nil, "", "", ""
	}
	analyzer, err := visionopenai.New(*visionProvider)
	if err != nil {
		slog.Error("failed to construct attachment vision client", "error", err)
		return nil, nil, "", "", ""
	}
	slog.Info("attachment search initialized", "embedding_model", embeddingModel, "vision_model", visionModel, "vision_provider", visionProvider.ID)
	return attachmentVectorStore, analyzer, visionProvider.ID, visionModel, imageCfg.GetPrompt()
}

// convertAIProviderTypeFromStore maps the store proto provider type to the ai package type.
func convertAIProviderTypeFromStore(providerType storepb.AIProviderType) ai.ProviderType {
	switch providerType {
	case storepb.AIProviderType_OPENAI:
		return ai.ProviderOpenAI
	case storepb.AIProviderType_GEMINI:
		return ai.ProviderGemini
	default:
		return ""
	}
}

func (s *Server) stopBackgroundRunners() {
	for _, cancelFunc := range s.backgroundRunnerCancels {
		if cancelFunc != nil {
			cancelFunc()
		}
	}
}

func (s *Server) waitBackgroundRunners(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		s.backgroundRunnerWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		select {
		case <-done:
			return
		default:
		}
		slog.Error("failed to stop background runners", slog.String("error", ctx.Err().Error()))
	}
}

func (s *Server) closeLongLivedConnections() {
	// Long-lived SSE requests do not finish on their own during http.Server.Shutdown.
	if s.sseHub != nil {
		s.sseHub.Close()
	}
}

func (s *Server) shutdownHTTPServer(ctx context.Context) {
	if s.httpServer == nil {
		return
	}
	if err := s.httpServer.Shutdown(ctx); err != nil {
		slog.Error("failed to shutdown server", slog.String("error", err.Error()))
		if closeErr := s.httpServer.Close(); closeErr != nil && closeErr != http.ErrServerClosed {
			slog.Error("failed to close server", slog.String("error", closeErr.Error()))
		}
	}
}

func (s *Server) getOrUpsertInstanceBasicSetting(ctx context.Context) (*storepb.InstanceBasicSetting, error) {
	instanceBasicSetting, err := s.Store.GetInstanceBasicSetting(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get instance basic setting")
	}
	modified := false
	if instanceBasicSetting.SecretKey == "" {
		instanceBasicSetting.SecretKey = uuid.NewString()
		modified = true
	}
	if modified {
		instanceSetting, err := s.Store.UpsertInstanceSetting(ctx, &storepb.InstanceSetting{
			Key:   storepb.InstanceSettingKey_BASIC,
			Value: &storepb.InstanceSetting_BasicSetting{BasicSetting: instanceBasicSetting},
		})
		if err != nil {
			return nil, errors.Wrap(err, "failed to upsert instance setting")
		}
		instanceBasicSetting = instanceSetting.GetBasicSetting()
	}
	return instanceBasicSetting, nil
}
