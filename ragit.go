// Package ragit is a reusable RAG ingestion pipeline: extract, chunk,
// embed, and store a document, ready for retrieval. See docs/design.md for
// the full design and the production reference implementation it's
// grounded in.
//
// [Processor.ProcessDocument] is resumable: an interrupted run picks up
// from whatever was already embedded in the current embedding space rather
// than re-billing every chunk on retry — see the package jryannel/ragit/jobs
// for wiring it into a River worker. Search, OCR, and the extraction
// fallback chain are later phases of docs/design.md, not forgotten.
package ragit

import (
	"context"
	"fmt"
	"io"
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
	// maxChunksPerDoc caps chunks per document before embedding is skipped.
	// 0 (the default) disables the cap. Set via WithMaxChunksPerDocument.
	maxChunksPerDoc int
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

// WithMaxChunksPerDocument sets the per-document chunk cap (0 = no cap) and
// returns the Processor for chaining. Above the cap, embedding is skipped
// and the document is flagged skipped_too_large instead of consuming the
// embedding budget.
func (p *Processor) WithMaxChunksPerDocument(n int) *Processor {
	p.maxChunksPerDoc = n
	return p
}

// embedBatchSize bounds how many chunks are embedded per provider call.
const embedBatchSize = 10

// CreateDocument stores data and inserts a pending documents row. Fast and
// synchronous — meant to be called from an upload handler, before a
// ProcessDocument job is enqueued.
func (p *Processor) CreateDocument(ctx context.Context, tenantID uuid.UUID, filename, mimeType string, data []byte) (uuid.UUID, error) {
	uri, err := p.store.Put(ctx, tenantID, filename, data, mimeType)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ragit: store document: %w", err)
	}

	doc, err := p.queries.CreateDocument(ctx, db.CreateDocumentParams{
		TenantID:  tenantID,
		SourceUri: &uri,
		Filename:  filename,
		MimeType:  mimeType,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("ragit: create document row: %w", err)
	}
	return doc.ID, nil
}

// ProcessDocument runs extract→chunk→embed→store for an existing document,
// resuming from whatever was already embedded in the current embedding
// space rather than re-billing chunks a prior attempt already paid for.
//
// The document always ends in status ready, error, or skipped_too_large.
// ProcessDocument still returns the underlying error on failure — callers
// (a River worker, or a direct caller) need it to decide whether the
// failure is worth retrying; it is not swallowed into a nil-error result.
func (p *Processor) ProcessDocument(ctx context.Context, documentID, tenantID uuid.UUID) error {
	doc, err := p.queries.GetDocumentByID(ctx, db.GetDocumentByIDParams{ID: documentID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("ragit: load document: %w", err)
	}

	if err := p.queries.UpdateDocumentProcessing(ctx, documentID); err != nil {
		return fmt.Errorf("ragit: mark document processing: %w", err)
	}

	if err := p.processDocument(ctx, &doc); err != nil {
		msg := err.Error()
		if updateErr := p.queries.UpdateDocumentError(ctx, db.UpdateDocumentErrorParams{ID: documentID, Error: &msg}); updateErr != nil {
			return fmt.Errorf("ragit: mark document error (after %v): %w", err, updateErr)
		}
		return err
	}
	return nil
}

func (p *Processor) processDocument(ctx context.Context, doc *db.Document) error {
	if doc.SourceUri == nil {
		return fmt.Errorf("ragit: document has no stored source")
	}
	reader, err := p.store.Get(ctx, *doc.SourceUri)
	if err != nil {
		return fmt.Errorf("ragit: retrieve stored document: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("ragit: read stored document: %w", err)
	}

	result, chunks, err := p.extractAndChunk(ctx, data, doc.Filename)
	if err != nil {
		return err
	}

	if p.maxChunksPerDoc > 0 && len(chunks) > p.maxChunksPerDoc {
		if err := p.queries.ClearDocumentChunks(ctx, db.ClearDocumentChunksParams{DocumentID: doc.ID, TenantID: doc.TenantID}); err != nil {
			return fmt.Errorf("ragit: clear chunks for skipped document: %w", err)
		}
		msg := fmt.Sprintf("document produced %d chunks, exceeding the %d-chunk limit; embedding skipped", len(chunks), p.maxChunksPerDoc)
		if err := p.queries.UpdateDocumentSkippedTooLarge(ctx, db.UpdateDocumentSkippedTooLargeParams{ID: doc.ID, Error: &msg}); err != nil {
			return fmt.Errorf("ragit: mark document skipped: %w", err)
		}
		return nil
	}

	if err := p.embedAndStore(ctx, doc.ID, doc.TenantID, chunks); err != nil {
		return err
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
		return fmt.Errorf("ragit: mark document ready: %w", err)
	}
	return nil
}

// Ingest is a synchronous convenience wrapper around CreateDocument +
// ProcessDocument, for callers that don't need async job processing. On
// failure it still returns the underlying error (see ProcessDocument);
// Document reflects the document's persisted state either way.
func (p *Processor) Ingest(ctx context.Context, tenantID uuid.UUID, filename, mimeType string, data []byte) (*Document, error) {
	documentID, err := p.CreateDocument(ctx, tenantID, filename, mimeType, data)
	if err != nil {
		return nil, err
	}

	processErr := p.ProcessDocument(ctx, documentID, tenantID)

	doc, err := p.queries.GetDocumentByID(ctx, db.GetDocumentByIDParams{ID: documentID, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("ragit: load document after processing: %w", err)
	}

	result := &Document{
		ID:       doc.ID,
		TenantID: doc.TenantID,
		Filename: doc.Filename,
		MimeType: doc.MimeType,
		Status:   doc.Status,
	}
	if doc.Error != nil {
		result.Error = *doc.Error
	}
	if doc.ChunkCount != nil {
		result.ChunkCount = int(*doc.ChunkCount)
	}
	if doc.EmbeddingModel != nil {
		result.EmbeddingModel = *doc.EmbeddingModel
	}
	return result, processErr
}

// DeleteDocument removes a document and its chunks (cascaded via the FK).
// It does not purge the original bytes from object storage — store.Store
// has no delete operation yet; that's a deliberate gap, not an oversight.
func (p *Processor) DeleteDocument(ctx context.Context, documentID, tenantID uuid.UUID) error {
	if err := p.queries.DeleteDocument(ctx, db.DeleteDocumentParams{ID: documentID, TenantID: tenantID}); err != nil {
		return fmt.Errorf("ragit: delete document: %w", err)
	}
	return nil
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

// embedAndStore embeds chunks not already embedded in the current embedding
// space and persists each batch as it completes. See the resume algorithm
// in docs/design.md §7: a persisted chunk is reused only if its fingerprint
// matches the live embedder AND its content matches the freshly re-chunked
// text at that index; any mismatch wipes every chunk for the document and
// starts clean, so embedding spaces never mix within one document.
func (p *Processor) embedAndStore(ctx context.Context, documentID, tenantID uuid.UUID, chunks []chunk.Chunk) error {
	currentFP := embed.Fingerprint(p.embedder)

	freshByIndex := make(map[int32]string, len(chunks))
	for _, c := range chunks {
		freshByIndex[int32(c.Index)] = c.Content
	}

	existing, err := p.queries.GetChunkDigestsByDocumentID(ctx, db.GetChunkDigestsByDocumentIDParams{DocumentID: documentID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("ragit: load existing chunks: %w", err)
	}

	embedded := make(map[int32]bool, len(existing))
	stale := false
	for _, e := range existing {
		if e.EmbeddingFingerprint != nil && *e.EmbeddingFingerprint == currentFP && freshByIndex[e.ChunkIndex] == e.Content {
			embedded[e.ChunkIndex] = true
		} else {
			stale = true
			break
		}
	}

	if stale {
		if err := p.queries.ClearDocumentChunks(ctx, db.ClearDocumentChunksParams{DocumentID: documentID, TenantID: tenantID}); err != nil {
			return fmt.Errorf("ragit: clear stale chunks: %w", err)
		}
		embedded = map[int32]bool{}
	}

	for start := 0; start < len(chunks); start += embedBatchSize {
		end := min(start+embedBatchSize, len(chunks))

		pending := make([]chunk.Chunk, 0, end-start)
		for _, c := range chunks[start:end] {
			if !embedded[int32(c.Index)] {
				pending = append(pending, c)
			}
		}
		if len(pending) == 0 {
			continue
		}

		texts := make([]string, len(pending))
		for i, c := range pending {
			texts[i] = c.Content
		}

		vectors, err := p.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed: batch %d: %w", start/embedBatchSize, err)
		}

		rows := make([]db.CreateChunksParams, len(pending))
		for i, c := range pending {
			vec := pgvector.NewVector(vectors[i])
			rows[i] = db.CreateChunksParams{
				DocumentID:           documentID,
				TenantID:             tenantID,
				ChunkIndex:           int32(c.Index),
				HeadingPath:          c.HeadingPath,
				Content:              c.Content,
				Embedding:            &vec,
				EmbeddingFingerprint: &currentFP,
				Metadata:             []byte("{}"),
			}
		}
		if _, err := p.queries.CreateChunks(ctx, rows); err != nil {
			return fmt.Errorf("store: batch %d: %w", start/embedBatchSize, err)
		}
	}
	return nil
}
