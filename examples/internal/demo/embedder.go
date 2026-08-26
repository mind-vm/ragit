// Package demo holds what both examples need in common, so the two main()s
// stay short enough to read side by side. Anything here that differs between
// them is a finding, not a helper.
package demo

import (
	"context"
	"sync/atomic"

	"github.com/mind-vm/ragit/embed"
)

// CountingEmbedder wraps an Embedder and counts what it was asked to do.
//
// It exists because the most important claim ragit makes about ingestion is a
// claim about *not* doing work: an interrupted or repeated run resumes from
// what was already embedded rather than re-billing the provider. That claim is
// invisible from the outside — a second run that silently re-embedded
// everything would produce identical output and a larger invoice.
//
// So the examples count calls and print the number. A resumed run must show
// zero.
type CountingEmbedder struct {
	// Embedded, not wrapped field-by-field: Provider/Model/Dimension are
	// forwarded unchanged, and they are half of embedding_fingerprint. A
	// decorator that altered them would silently create a second embedding
	// space.
	embed.Embedder

	calls atomic.Int64
	texts atomic.Int64
}

var _ embed.Embedder = (*CountingEmbedder)(nil)

// NewCounting wraps e.
func NewCounting(e embed.Embedder) *CountingEmbedder {
	return &CountingEmbedder{Embedder: e}
}

// Embed counts the call and the texts in it, then delegates.
func (c *CountingEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	c.calls.Add(1)
	c.texts.Add(int64(len(texts)))
	return c.Embedder.Embed(ctx, texts)
}

// Calls is how many provider round-trips happened.
func (c *CountingEmbedder) Calls() int64 { return c.calls.Load() }

// Texts is how many individual texts were embedded across those calls.
func (c *CountingEmbedder) Texts() int64 { return c.texts.Load() }

// Reset zeroes both counters, so a phase of the example can be measured on its
// own.
func (c *CountingEmbedder) Reset() {
	c.calls.Store(0)
	c.texts.Store(0)
}
