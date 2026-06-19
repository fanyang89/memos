package ai

import "github.com/pkg/errors"

const (
	// DefaultOpenAITranscriptionModel is the built-in OpenAI transcription model.
	DefaultOpenAITranscriptionModel = "whisper-1"
	// DefaultGeminiTranscriptionModel is the built-in Gemini transcription model.
	DefaultGeminiTranscriptionModel = "gemini-2.5-flash"
	// DefaultOpenAIEmbeddingModel is the built-in OpenAI embedding model.
	DefaultOpenAIEmbeddingModel = "text-embedding-3-small"
	// DefaultOpenAIVisionModel is the built-in OpenAI-compatible vision model.
	// For Ollama deployments, users should usually configure a local model such as qwen3-vl:8b.
	DefaultOpenAIVisionModel = "qwen3-vl:8b"
)

// DefaultTranscriptionModel returns the built-in transcription model for a provider.
func DefaultTranscriptionModel(providerType ProviderType) (string, error) {
	switch providerType {
	case ProviderOpenAI:
		return DefaultOpenAITranscriptionModel, nil
	case ProviderGemini:
		return DefaultGeminiTranscriptionModel, nil
	default:
		return "", errors.Wrapf(ErrCapabilityUnsupported, "provider type %q", providerType)
	}
}

// DefaultEmbeddingModel returns the built-in embedding model for a provider.
// Semantic search currently only supports OpenAI-compatible providers (which
// includes Ollama via its OpenAI-compat layer); Gemini embeddings are not wired.
func DefaultEmbeddingModel(providerType ProviderType) (string, error) {
	switch providerType {
	case ProviderOpenAI:
		return DefaultOpenAIEmbeddingModel, nil
	case ProviderGemini:
		return "", errors.Wrap(ErrCapabilityUnsupported, "gemini embeddings not wired")
	default:
		return "", errors.Wrapf(ErrCapabilityUnsupported, "provider type %q", providerType)
	}
}

// DefaultVisionModel returns the built-in vision model for a provider.
func DefaultVisionModel(providerType ProviderType) (string, error) {
	switch providerType {
	case ProviderOpenAI:
		return DefaultOpenAIVisionModel, nil
	case ProviderGemini:
		return "", errors.Wrap(ErrCapabilityUnsupported, "gemini image search not wired")
	default:
		return "", errors.Wrapf(ErrCapabilityUnsupported, "provider type %q", providerType)
	}
}
