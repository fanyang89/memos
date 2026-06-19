// Package embeddings defines an Embedder that produces dense vector
// representations of text for semantic search.
package embeddings

import "context"

// Embedder produces embeddings for batches of text.
type Embedder interface {
	// Embed returns one normalized []float32 per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}
