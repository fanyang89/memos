// Package openai implements embeddings.Embedder against the OpenAI
// /v1/embeddings endpoint (and any compatible third-party endpoint such as
// Ollama's OpenAI-compat layer, Azure, or a self-hosted inference server).
package openai

import (
	"context"
	"net/url"
	"strings"

	openaisdk "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/pkg/errors"

	"github.com/usememos/memos/internal/ai"
	"github.com/usememos/memos/internal/ai/embeddings"
)

const defaultEndpoint = "https://api.openai.com/v1"

// Embedder implements embeddings.Embedder for OpenAI-compatible embedding endpoints.
type Embedder struct {
	client openaisdk.Client
	model  openaisdk.EmbeddingModel
}

// Options carries optional dependencies for constructing an Embedder.
type Options struct {
	// HTTPClient overrides the default SDK HTTP client (useful for tests).
	HTTPClient openaioption.RequestOption
}

// New constructs an Embedder from a provider config and a model identifier.
// The model is fixed at construction time because the chromem-go collection
// (and its vector dimensionality) is bound to the model.
func New(cfg ai.ProviderConfig, model string, options ...openaioption.RequestOption) (*Embedder, error) {
	endpoint, err := normalizeEndpoint(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	if cfg.APIKey == "" {
		return nil, errors.New("OpenAI API key is required")
	}
	if strings.TrimSpace(model) == "" {
		return nil, errors.New("embedding model is required")
	}
	return &Embedder{
		client: openaisdk.NewClient(
			append(
				[]openaioption.RequestOption{
					openaioption.WithAPIKey(cfg.APIKey),
					openaioption.WithBaseURL(endpoint),
				},
				options...,
			)...,
		),
		model: openaisdk.EmbeddingModel(model),
	}, nil
}

// Embed returns one embedding vector per input text, in input order.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	resp, err := e.client.Embeddings.New(ctx, openaisdk.EmbeddingNewParams{
		Input: openaisdk.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: texts,
		},
		Model: e.model,
	})
	if err != nil {
		return nil, errors.Wrap(err, "failed to call OpenAI embeddings endpoint")
	}
	out := make([][]float32, len(texts))
	for _, item := range resp.Data {
		idx := int(item.Index)
		if idx < 0 || idx >= len(out) {
			return nil, errors.Wrapf(err, "embedding index %d out of range [0,%d)", idx, len(out))
		}
		vec := make([]float32, len(item.Embedding))
		for i, v := range item.Embedding {
			vec[i] = float32(v)
		}
		out[idx] = vec
	}
	for i, vec := range out {
		if vec == nil {
			return nil, errors.Errorf("missing embedding for input index %d", i)
		}
	}
	return out, nil
}

// Compile-time interface check.
var _ embeddings.Embedder = (*Embedder)(nil)

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	if _, err := url.ParseRequestURI(endpoint); err != nil {
		return "", errors.Wrap(err, "invalid OpenAI endpoint")
	}
	return strings.TrimRight(endpoint, "/"), nil
}
