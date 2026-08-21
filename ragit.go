// Package ragit is a reusable RAG ingestion pipeline: extract, chunk,
// embed, and store a document, ready for retrieval. See docs/design.md for
// the full design and the production reference implementation it's
// grounded in.
//
// This is a walking skeleton: [Processor.Ingest] runs synchronously and has
// no resumable-job wrapping, search, OCR, or extraction-fallback-chain yet
// — those are later phases of docs/design.md, not forgotten.
package ragit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/extract"
	"github.com/jryannel/ragit/internal/db"
	"github.com/jryannel/ragit/store"
)

// Document mirrors the persisted documents row, in the package's own type
// rather than leaking the sqlc-generated one across the public API.
type Document struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	Filename       string
	MimeType       string
	Status         string
	Error          string
	ChunkCount     int
	EmbeddingModel string
}

// Processor wires extraction, chunking, embedding, and storage into one
// ingestion pipeline.
type Processor struct {
	pool      *pgxpool.Pool
	queries   *db.Queries
	extractor extract.Extractor
	chunker   *chunk.Chunker
	embedder  embed.Embedder
	store     store.Store
}

// New builds a Processor. The caller owns pool/store's lifecycle.
func New(pool *pgxpool.Pool, extractor extract.Extractor, chunker *chunk.Chunker, embedder embed.Embedder, st store.Store) *Processor {
	return &Processor{
		pool:      pool,
		queries:   db.New(pool),
		extractor: extractor,
		chunker:   chunker,
		embedder:  embedder,
		store:     st,
	}
}

// embedBatchSize bounds how many chunks are embedded per provider call.
const embedBatchSize = 10

// Ingest stores, extracts, chunks, and embeds one document, synchronously.
func (p *Processor) Ingest(ctx context.Context, tenantID uuid.UUID, filename, mimeType string, data []byte) (*Document, error) {
	uri, err := p.store.Put(ctx, tenantID, filename, data, mimeType)
	if err != nil {
		return nil, fmt.Errorf("ragit: store document: %w", err)
	}

	doc, err := p.queries.CreateDocument(ctx, db.CreateDocumentParams{
		TenantID:  tenantID,
		SourceUri: &uri,
		Filename:  filename,
		MimeType:  mimeType,
	})
	if err != nil {
		return nil, fmt.Errorf("ragit: create document row: %w", err)
	}

	if err := p.queries.UpdateDocumentProcessing(ctx, doc.ID); err != nil {
		return nil, fmt.Errorf("ragit: mark document processing: %w", err)
	}

	result, chunks, err := p.extractAndChunk(ctx, data, filename)
	if err != nil {
		return p.fail(ctx, doc.ID, tenantID, filename, mimeType, err)
	}

	if err := p.embedAndStore(ctx, doc.ID, tenantID, chunks); err != nil {
		return p.fail(ctx, doc.ID, tenantID, filename, mimeType, err)
	}

	metadata := result.Metadata
	if metadata == nil {
		metadata = []byte("{}")
	}
	chunkCount := int32(len(chunks))
	model := p.embedder.Model()
	now := time.Now()
	if err := p.queries.UpdateDocumentReady(ctx, db.UpdateDocumentReadyParams{
		ID:             doc.ID,
		TextContent:    &result.Text,
		Metadata:       metadata,
		ChunkCount:     &chunkCount,
		EmbeddingModel: &model,
		ProcessedAt:    &now,
	}); err != nil {
		return nil, fmt.Errorf("ragit: mark document ready: %w", err)
	}

	return &Document{
		ID:             doc.ID,
		TenantID:       tenantID,
		Filename:       filename,
		MimeType:       mimeType,
		Status:         "ready",
		ChunkCount:     len(chunks),
		EmbeddingModel: model,
	}, nil
}

func (p *Processor) extractAndChunk(ctx context.Context, data []byte, filename string) (*extract.Result, []chunk.Chunk, error) {
	result, err := p.extractor.Extract(ctx, data, filename)
	if err != nil {
		return nil, nil, fmt.Errorf("extract: %w", err)
	}
	if result.Text == "" {
		return nil, nil, fmt.Errorf("extract: no text content extracted")
	}

	chunks := p.chunker.SplitMarkdown(result.Text)
	if len(chunks) == 0 {
		return nil, nil, fmt.Errorf("chunk: no chunks produced")
	}
	return result, chunks, nil
}

func (p *Processor) embedAndStore(ctx context.Context, documentID, tenantID uuid.UUID, chunks []chunk.Chunk) error {
	fingerprint := embed.Fingerprint(p.embedder)

	for start := 0; start < len(chunks); start += embedBatchSize {
		end := min(start+embedBatchSize, len(chunks))
		batch := chunks[start:end]

		texts := make([]string, len(batch))
		for i, c := range batch {
			texts[i] = c.Content
		}

		vectors, err := p.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed: batch %d: %w", start/embedBatchSize, err)
		}

		rows := make([]db.CreateChunksParams, len(batch))
		for i, c := range batch {
			vec := pgvector.NewVector(vectors[i])
			rows[i] = db.CreateChunksParams{
				DocumentID:           documentID,
				TenantID:             tenantID,
				ChunkIndex:           int32(c.Index),
				HeadingPath:          c.HeadingPath,
				Content:              c.Content,
				Embedding:            &vec,
				EmbeddingFingerprint: &fingerprint,
				Metadata:             []byte("{}"),
			}
		}
		if _, err := p.queries.CreateChunks(ctx, rows); err != nil {
			return fmt.Errorf("store: batch %d: %w", start/embedBatchSize, err)
		}
	}
	return nil
}

func (p *Processor) fail(ctx context.Context, documentID, tenantID uuid.UUID, filename, mimeType string, cause error) (*Document, error) {
	msg := cause.Error()
	if err := p.queries.UpdateDocumentError(ctx, db.UpdateDocumentErrorParams{ID: documentID, Error: &msg}); err != nil {
		return nil, fmt.Errorf("ragit: mark document error (after %v): %w", cause, err)
	}
	return &Document{
		ID:       documentID,
		TenantID: tenantID,
		Filename: filename,
		MimeType: mimeType,
		Status:   "error",
		Error:    msg,
	}, nil
}
