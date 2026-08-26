package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/embed"
)

// storePrepared writes chunks that arrived already chunked and already
// embedded.
//
// # This is the bypass, and it is the point of the example
//
// ragit has no seam for this. Processor.ProcessDocument runs
// extract → chunk → embed as one sealed sequence: the Extractor returns a flat
// string, the chunker is a concrete *chunk.Chunker rather than an interface,
// and embedAndStore always calls p.embedder. There is no way to say "the chunks
// and the vectors already exist, just persist them".
//
// So this reaches past the Processor and writes ragit.Chunk rows with sqlb,
// which is the escape hatch the README advertises — for reads. Whether it holds
// up for writes is what this example measures. Everything ProcessDocument would
// have done for free and this has to redo by hand is marked FRICTION below.
func storePrepared(
	ctx context.Context,
	pool *pgxpool.Pool,
	processor *ragit.Processor,
	tenantID, documentID uuid.UUID,
	result *Result,
	embedder embed.Embedder,
) error {
	// FRICTION 1: the denormalized columns have to be read back off the
	// document and copied onto every chunk — tenant, both scope dimensions,
	// the session, the attributes, the retention clock. ragit denormalizes
	// them so retrieval never needs a join, and nothing enforces that a
	// hand-written insert does the same. Forget `attributes` and attribute
	// filtering silently returns nothing; forget `scope_a_id` and the chunks
	// answer searches for the wrong scope. Both failures are quiet.
	doc, err := processor.GetDocument(ctx, ragit.Tenant(tenantID), documentID)
	if err != nil {
		return fmt.Errorf("read document back for denormalization: %w", err)
	}

	fingerprint := embed.Fingerprint(embedder)

	rows := make([]*ragit.Chunk, 0, len(result.Chunks))
	for _, c := range result.Chunks {
		vec := sqlb.Vector(c.Embedding)

		// xberg's chunk_type has no column in ragit's schema, so it goes in
		// the chunk metadata rather than being dropped.
		meta, err := json.Marshal(map[string]any{
			"chunk_type": c.ChunkType,
			"chunker":    "xberg/markdown",
		})
		if err != nil {
			return err
		}

		rows = append(rows, &ragit.Chunk{
			DocumentID:  documentID,
			TenantID:    doc.TenantID,
			ScopeAID:    doc.ScopeAID,
			ScopeBID:    doc.ScopeBID,
			SessionID:   doc.SessionID,
			ChunkIndex:  int32(c.Index),
			HeadingPath: c.HeadingPath,
			Content:     c.Content,
			Embedding:   &vec,
			// FRICTION 2: the fingerprint is the caller's to get right. It is
			// what VectorSearch filters on, so a mismatch between what is
			// written here and what the query embedder reports does not error
			// — it returns zero results, which is indistinguishable from an
			// empty corpus. One object supplies both here, deliberately.
			EmbeddingFingerprint: &fingerprint,
			Metadata:             meta,
			Attributes:           doc.Attributes,
			ExpiresAt:            doc.ExpiresAt,
		})
	}

	now := time.Now()
	model := embedder.Model()
	count := int32(len(rows))

	return ragit.WithTenant(ctx, pool, tenantID, func(db sqlb.Executor) error {
		// FRICTION 3: reprocessing is the caller's problem. ragit's
		// embedAndStore reuses chunks whose fingerprint AND content still
		// match, so a retry resumes instead of re-billing. There is no way to
		// reach that logic from out here, so this deletes and rewrites every
		// time. With xberg's local ONNX embedder that costs CPU rather than
		// money — but the guard is gone, not merely cheaper.
		if _, err := db.Exec(ctx,
			"DELETE FROM ragit_chunks WHERE document_id = $1", documentID); err != nil {
			return fmt.Errorf("clear chunks: %w", err)
		}

		if len(rows) > 0 {
			if _, err := sqlb.InsertRows(rows...).
				Omit("id", "created_at").
				Exec(ctx, db); err != nil {
				return fmt.Errorf("insert chunks: %w", err)
			}
		}

		// FRICTION 4: the terminal state is hand-written. Processor.finish is
		// unexported, so every column it sets has to be set again here —
		// status, text_content, metadata, chunk_count, embedding_model,
		// processed_at — from outside, with nothing checking the set is
		// complete. Miss one and the catalog quietly lies: a document that is
		// fully indexed but still reads `pending`, or reads `ready` with a
		// null chunk_count.
		metadata := result.Metadata
		if len(metadata) == 0 {
			metadata = json.RawMessage("{}")
		}
		if _, err := sqlb.UpdateRows[ragit.Document]().
			Where(sqlb.RawPred("id = ?", documentID)).
			Set("status", ragit.StatusReady).
			Set("text_content", &result.Content).
			Set("metadata", metadata).
			Set("chunk_count", count).
			Set("embedding_model", &model).
			Set("processed_at", &now).
			Exec(ctx, db); err != nil {
			return fmt.Errorf("mark document ready: %w", err)
		}
		return nil
	})

	// FRICTION 5, not visible above because there is nothing to write: the
	// EventSink never fires. Processor.finish publishes on every terminal
	// state, and a host application subscribing to indexing events simply
	// stops hearing about documents that came in through this path.
}
