package main

import (
	"context"
	"fmt"

	"github.com/mind-vm/ragit/embed"
)

// xbergEmbedder puts xberg's embedder behind ragit's Embedder interface, so
// VectorSearch can embed a query into the same space the corpus lives in.
//
// # Why this is awkward, and what the awkwardness is evidence of
//
// The corpus is embedded as a side effect of extraction — chunks come back from
// /extract with vectors attached, and nothing here is involved. But a search
// query is not a document, and ragit still needs it turned into a vector that is
// comparable with those chunks. xberg has no HTTP endpoint for embedding text:
// `embed_texts()` exists in its language bindings, `xberg embed` exists as a CLI
// command, and the REST server exposes neither.
//
// So a query gets posted to /extract as a one-chunk pseudo-document and the
// vector is read off the chunk that comes back. It works, and it costs a full
// extraction round trip — MIME detection, format dispatch, chunking — to embed
// six words.
//
// That is the sharpest evidence this example produces about ragit's own shape.
// One Embedder serving both corpus and query is the right constraint when the
// application owns embedding, because it is what keeps the two in one space. It
// is the wrong constraint when the corpus is embedded by something that has no
// text-embedding API at all. See docs/examples-plan.md, shape question 3.
type xbergEmbedder struct {
	client *Client
}

var _ embed.Embedder = (*xbergEmbedder)(nil)

func newXbergEmbedder(client *Client) *xbergEmbedder {
	return &xbergEmbedder{client: client}
}

// Embed embeds each text with its own round trip. There is no batch form: the
// endpoint takes one document.
func (e *xbergEmbedder) Embed(ctx context.Context, texts []string) ([]embed.Vector, error) {
	out := make([]embed.Vector, 0, len(texts))
	for i, text := range texts {
		vec, err := e.client.EmbedText(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("embed text %d: %w", i, err)
		}
		out = append(out, embed.Vector(vec))
	}
	return out, nil
}

// Provider, Model and Dimension are the three parts of the embedding
// fingerprint, and every one of them is asserted rather than observed.
//
// xberg's response says nothing about which model produced the vectors — not in
// the chunk, not in the result metadata. So these constants are a promise this
// program makes on xberg's behalf. Change the preset and the fingerprint keeps
// claiming BGE-base, and the corpus straddles two embedding spaces while
// looking like one.
func (e *xbergEmbedder) Provider() string { return EmbeddingProvider }
func (e *xbergEmbedder) Model() string    { return EmbeddingModel }
func (e *xbergEmbedder) Dimension() int   { return EmbeddingDimension }
