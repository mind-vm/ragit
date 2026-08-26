package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/mind-vm/ragit"
)

// storePrepared hands xberg's output to ragit.
//
// # This file used to be the point of the example
//
// It held 143 lines and five numbered FRICTION comments. ragit had no seam for
// a corpus it had not chunked or embedded: Processor.ProcessDocument ran
// extract → chunk → embed as one sealed sequence, so this reached past the
// Processor and wrote ragit.Chunk rows with sqlb — the escape hatch the README
// advertises for reads, used here for writes. It worked, and it cost:
//
//  1. every denormalized column copied onto every chunk by hand — tenant, both
//     scopes, session, attributes, expiry — each silent when forgotten
//  2. the embedding fingerprint formatted by the caller, where a mismatch with
//     the query embedder returns zero results rather than an error
//  3. no resume guard, so every pass deleted and rewrote the whole corpus
//  4. the terminal state set column by column, Processor.finish being
//     unexported, with nothing checking the set was complete
//  5. the EventSink never firing, so a host application's catalog never heard
//     about a document that came in this way
//
// ragit.IngestPrepared owns all five. What is left here is the one thing that
// is genuinely this program's job: mapping xberg's response onto ragit's type.
// Read the original at git log -- examples/xberg-owned/ingest.go — the
// before/after is the example's real finding.
func storePrepared(
	ctx context.Context,
	processor *ragit.Processor,
	tenantID, documentID uuid.UUID,
	result *Result,
) error {
	prepared, err := preparedFrom(result)
	if err != nil {
		return err
	}
	return processor.IngestPrepared(ctx, documentID, tenantID, prepared)
}

// preparedFrom maps one xberg /extract response onto ragit's shape.
func preparedFrom(result *Result) (ragit.PreparedDocument, error) {
	chunks := make([]ragit.PreparedChunk, len(result.Chunks))
	for i, c := range result.Chunks {
		// xberg's chunk_type has no column in ragit's schema, so it goes in
		// the chunk metadata rather than being dropped. That the metadata
		// survives at all is new: the hand-written path had a place to put it
		// too, but page spans and byte offsets went nowhere on the way in.
		meta, err := json.Marshal(map[string]any{
			"chunk_type": c.ChunkType,
			"chunker":    "xberg/markdown",
		})
		if err != nil {
			return ragit.PreparedDocument{}, fmt.Errorf("chunk %d metadata: %w", i, err)
		}

		chunks[i] = ragit.PreparedChunk{
			Content:     c.Content,
			Embedding:   c.Embedding,
			HeadingPath: c.HeadingPath,
			Metadata:    meta,
		}
	}

	return ragit.PreparedDocument{
		Text:     result.Content,
		Metadata: result.Metadata,
		Space:    EmbeddingSpace,
		Chunks:   chunks,
	}, nil
}
