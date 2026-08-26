// Package embed turns text into vectors via a single client speaking the
// OpenAI embeddings wire format — not per-provider adapters. That format is
// a de-facto standard: OpenAI's own API, and any "OpenAI-compatible"
// gateway in front of another provider (this package defaults to EdenAI's
// EU gateway), speak it identically, so switching provider is a config
// change rather than a new adapter.
package embed

import (
	"context"
)

// Vector is one embedding.
type Vector []float32

// Embedder turns text into vectors.
type Embedder interface {
	// Embed embeds a batch of texts, preserving order.
	Embed(ctx context.Context, texts []string) ([]Vector, error)
	// Provider identifies the backend, e.g. "edenai".
	Provider() string
	// Model is the active embedding model id.
	Model() string
	// Dimension is the embedding width stored in pgvector.
	Dimension() int
}
