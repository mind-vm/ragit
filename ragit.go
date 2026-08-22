// Package ragit is a reusable RAG pipeline: extract, chunk, embed, and store
// a document, then retrieve it. See docs/design.md for the full design and
// the production reference implementation it's grounded in.
//
// Two properties are worth knowing before wiring this in:
//
// [Processor.ProcessDocument] is resumable. An interrupted run picks up from
// whatever was already embedded in the current embedding space rather than
// re-billing every chunk on retry — see the jobs package for running it under
// River.
//
// Every query runs inside a transaction scoped to one tenant, because the
// tables carry FORCE ROW LEVEL SECURITY. That means tenant isolation is
// enforced by the database rather than by remembering a WHERE clause — but
// only if the application connects as a non-superuser role, since PostgreSQL
// exempts superusers from RLS entirely. See [db.WithTenant].
//
// OCR and the extraction fallback chain are later phases of docs/design.md,
// not forgotten.
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
	"github.com/jryannel/ragit/search"
	"github.com/jryannel/ragit/store"
)

// Document mirrors the persisted ragit_documents row, in the package's own
// type rather than leaking the sqlc-generated one across the public API.
type Document struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	ScopeID        *uuid.UUID
	SessionID      *uuid.UUID
	Filename       string
	MimeType       string
	Status         string
	Error          string
	ChunkCount     int
	EmbeddingModel string
}

// DocumentInput describes a document to ingest.
type DocumentInput struct {
	// TenantID is required; it is the security boundary enforced by RLS.
	TenantID uuid.UUID
	// ScopeID optionally files the document under a nested scope (a project,
	// workspace, or whatever the host app calls it). Reserved column: search
	// filters on it exactly, without the cascade described in design.md §8.
	ScopeID *uuid.UUID
	// SessionID marks the document as an ephemeral attachment belonging to
	// one conversation or agent session. Such documents are excluded from
	// ordinary library search unless a caller asks for that session by id.
	SessionID *uuid.UUID
	Filename  string
	MimeType  string
	Data      []byte
}

// Processor wires extraction, chunking, embedding, storage, and retrieval
// into one pipeline.
type Processor struct {
	pool      *pgxpool.Pool
	extractor extract.Extractor
	chunker   *chunk.Chunker
	embedder  embed.Embedder
	store     store.Store
	searcher  *search.Searcher
	// maxChunksPerDoc caps chunks per document before embedding is skipped.
	// 0 (the default) disables the cap. Set via WithMaxChunksPerDocument.
	maxChunksPerDoc int
}

// New builds a Processor. The caller owns pool/store's lifecycle.
func New(pool *pgxpool.Pool, extractor extract.Extractor, chunker *chunk.Chunker, embedder embed.Embedder, st store.Store) *Processor {
	return &Processor{
		pool:      pool,
		extractor: extractor,
		chunker:   chunker,
		embedder:  embedder,
		store:     st,
		searcher:  search.New(pool, embedder),
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

// Searcher exposes retrieval for callers that want the search API directly.
func (p *Processor) Searcher() *search.Searcher { return p.searcher }

// VectorSearch returns the chunks nearest to query by cosine similarity.
func (p *Processor) VectorSearch(ctx context.Context, tenantID uuid.UUID, query string, opts search.Options) ([]search.Result, error) {
	return p.searcher.Vector(ctx, tenantID, query, opts)
}

// FullTextSearch returns chunks matching query via Postgres full-text search.
// It is kept separate from VectorSearch rather than fused — see the search
// package documentation.
func (p *Processor) FullTextSearch(ctx context.Context, tenantID uuid.UUID, query string, opts search.Options) ([]search.Result, error) {
	return p.searcher.FullText(ctx, tenantID, query, opts)
}

// withTenant runs fn against a tenant-scoped transaction.
func (p *Processor) withTenant(ctx context.Context, tenantID uuid.UUID, fn func(*db.Queries) error) error {
	return db.WithTenant(ctx, p.pool, tenantID, fn)
}

// embedBatchSize bounds how many chunks are embedded per provider call.
const embedBatchSize = 10

// CreateDocument stores the bytes and inserts a pending ragit_documents row.
// Fast and synchronous — meant to be called from an upload handler, before a
// ProcessDocument job is enqueued.
func (p *Processor) CreateDocument(ctx context.Context, in DocumentInput) (uuid.UUID, error) {
	uri, err := p.store.Put(ctx, in.TenantID, in.Filename, in.Data, in.MimeType)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ragit: store document: %w", err)
	}

	var documentID uuid.UUID
	err = p.withTenant(ctx, in.TenantID, func(q *db.Queries) error {
		doc, err := q.CreateDocument(ctx, db.CreateDocumentParams{
			TenantID:  in.TenantID,
			ScopeID:   in.ScopeID,
			SessionID: in.SessionID,
			SourceUri: &uri,
			Filename:  in.Filename,
			MimeType:  in.MimeType,
		})
		if err != nil {
			return fmt.Errorf("ragit: create document row: %w", err)
		}
		documentID = doc.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return documentID, nil
}

// ProcessDocument runs extract→chunk→embed→store for an existing document,
// resuming from whatever was already embedded in the current embedding
// space rather than re-billing chunks a prior attempt already paid for.
//
// The document always ends in status ready, error, or skipped_too_large.
// ProcessDocument still returns the underlying error on failure — callers
// (a River worker, or a direct caller) need it to decide whether the
// failure is worth retrying; it is not swallowed into a nil-error result.
//
// The work is split across several short transactions rather than held in
// one. That is deliberate: a single transaction spanning the extractor and
// embedding provider's HTTP calls would hold a connection open for the whole
// run, and — worse — would make the per-batch checkpointing in embedAndStore
// meaningless, since nothing would be durable until the final commit.
func (p *Processor) ProcessDocument(ctx context.Context, documentID, tenantID uuid.UUID) error {
	var doc db.Document
	err := p.withTenant(ctx, tenantID, func(q *db.Queries) error {
		var err error
		doc, err = q.GetDocumentByID(ctx, db.GetDocumentByIDParams{ID: documentID, TenantID: tenantID})
		if err != nil {
			return fmt.Errorf("ragit: load document: %w", err)
		}
		if err := q.UpdateDocumentProcessing(ctx, documentID); err != nil {
			return fmt.Errorf("ragit: mark document processing: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	if err := p.processDocument(ctx, &doc); err != nil {
		msg := err.Error()
		updateErr := p.withTenant(ctx, tenantID, func(q *db.Queries) error {
			return q.UpdateDocumentError(ctx, db.UpdateDocumentErrorParams{ID: documentID, Error: &msg})
		})
		if updateErr != nil {
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
		msg := fmt.Sprintf("document produced %d chunks, exceeding the %d-chunk limit; embedding skipped", len(chunks), p.maxChunksPerDoc)
		return p.withTenant(ctx, doc.TenantID, func(q *db.Queries) error {
			if err := q.ClearDocumentChunks(ctx, db.ClearDocumentChunksParams{DocumentID: doc.ID, TenantID: doc.TenantID}); err != nil {
				return fmt.Errorf("ragit: clear chunks for skipped document: %w", err)
			}
			if err := q.UpdateDocumentSkippedTooLarge(ctx, db.UpdateDocumentSkippedTooLargeParams{ID: doc.ID, Error: &msg}); err != nil {
				return fmt.Errorf("ragit: mark document skipped: %w", err)
			}
			return nil
		})
	}

	if err := p.embedAndStore(ctx, doc, chunks); err != nil {
		return err
	}

	metadata := result.Metadata
	if metadata == nil {
		metadata = []byte("{}")
	}
	chunkCount := int32(len(chunks))
	model := p.embedder.Model()
	now := time.Now()
	return p.withTenant(ctx, doc.TenantID, func(q *db.Queries) error {
		if err := q.UpdateDocumentReady(ctx, db.UpdateDocumentReadyParams{
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
	})
}

// Ingest is a synchronous convenience wrapper around CreateDocument +
// ProcessDocument, for callers that don't need async job processing. On
// failure it still returns the underlying error (see ProcessDocument);
// Document reflects the document's persisted state either way.
func (p *Processor) Ingest(ctx context.Context, in DocumentInput) (*Document, error) {
	documentID, err := p.CreateDocument(ctx, in)
	if err != nil {
		return nil, err
	}

	processErr := p.ProcessDocument(ctx, documentID, in.TenantID)

	var doc db.Document
	if err := p.withTenant(ctx, in.TenantID, func(q *db.Queries) error {
		var err error
		doc, err = q.GetDocumentByID(ctx, db.GetDocumentByIDParams{ID: documentID, TenantID: in.TenantID})
		return err
	}); err != nil {
		return nil, fmt.Errorf("ragit: load document after processing: %w", err)
	}

	result := &Document{
		ID:        doc.ID,
		TenantID:  doc.TenantID,
		ScopeID:   doc.ScopeID,
		SessionID: doc.SessionID,
		Filename:  doc.Filename,
		MimeType:  doc.MimeType,
		Status:    doc.Status,
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

// MoveDocumentScope reassigns a document to a different scope and re-stamps
// its chunks to match.
//
// The resync is the whole point of this method existing rather than callers
// updating the document row themselves: chunks carry denormalized copies of
// scope_id/session_id so that search never needs a join, and those copies do
// not self-heal. Reprocessing the document does not fix them either — the
// resume check in ProcessDocument sees identical content, skips the rewrite,
// and leaves the chunks answering searches for their old scope.
func (p *Processor) MoveDocumentScope(ctx context.Context, documentID, tenantID uuid.UUID, scopeID, sessionID *uuid.UUID) error {
	return p.withTenant(ctx, tenantID, func(q *db.Queries) error {
		if err := q.UpdateDocumentScope(ctx, db.UpdateDocumentScopeParams{
			ID:        documentID,
			ScopeID:   scopeID,
			SessionID: sessionID,
		}); err != nil {
			return fmt.Errorf("ragit: update document scope: %w", err)
		}
		if err := q.ResyncChunkScope(ctx, db.ResyncChunkScopeParams{
			DocumentID: documentID,
			TenantID:   tenantID,
			ScopeID:    scopeID,
			SessionID:  sessionID,
		}); err != nil {
			return fmt.Errorf("ragit: resync chunk scope: %w", err)
		}
		return nil
	})
}

// DeleteDocument removes a document and its chunks (cascaded via the FK).
// It does not purge the original bytes from object storage — store.Store
// has no delete operation yet; that's a deliberate gap, not an oversight.
func (p *Processor) DeleteDocument(ctx context.Context, documentID, tenantID uuid.UUID) error {
	return p.withTenant(ctx, tenantID, func(q *db.Queries) error {
		if err := q.DeleteDocument(ctx, db.DeleteDocumentParams{ID: documentID, TenantID: tenantID}); err != nil {
			return fmt.Errorf("ragit: delete document: %w", err)
		}
		return nil
	})
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
// space and persists each batch as it completes, in its own transaction, so
// an interrupted run keeps everything it already paid for.
//
// See the resume algorithm in docs/design.md §7: a persisted chunk is reused
// only if its fingerprint matches the live embedder AND its content matches
// the freshly re-chunked text at that index; any mismatch wipes every chunk
// for the document and starts clean, so embedding spaces never mix within
// one document.
func (p *Processor) embedAndStore(ctx context.Context, doc *db.Document, chunks []chunk.Chunk) error {
	currentFP := embed.Fingerprint(p.embedder)

	freshByIndex := make(map[int32]string, len(chunks))
	for _, c := range chunks {
		freshByIndex[int32(c.Index)] = c.Content
	}

	embedded := make(map[int32]bool, len(chunks))
	if err := p.withTenant(ctx, doc.TenantID, func(q *db.Queries) error {
		existing, err := q.GetChunkDigestsByDocumentID(ctx, db.GetChunkDigestsByDocumentIDParams{
			DocumentID: doc.ID, TenantID: doc.TenantID,
		})
		if err != nil {
			return fmt.Errorf("ragit: load existing chunks: %w", err)
		}

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
			if err := q.ClearDocumentChunks(ctx, db.ClearDocumentChunksParams{DocumentID: doc.ID, TenantID: doc.TenantID}); err != nil {
				return fmt.Errorf("ragit: clear stale chunks: %w", err)
			}
			embedded = map[int32]bool{}
		}
		return nil
	}); err != nil {
		return err
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
		if len(vectors) != len(pending) {
			return fmt.Errorf("embed: batch %d: got %d vectors for %d chunks", start/embedBatchSize, len(vectors), len(pending))
		}

		rows := make([]db.CreateChunksParams, len(pending))
		for i, c := range pending {
			vec := pgvector.NewVector(vectors[i])
			rows[i] = db.CreateChunksParams{
				DocumentID:           doc.ID,
				TenantID:             doc.TenantID,
				ScopeID:              doc.ScopeID,
				SessionID:            doc.SessionID,
				ChunkIndex:           int32(c.Index),
				HeadingPath:          c.HeadingPath,
				Content:              c.Content,
				Embedding:            &vec,
				EmbeddingFingerprint: &currentFP,
				Metadata:             []byte("{}"),
			}
		}

		batchIdx := start / embedBatchSize
		if err := p.withTenant(ctx, doc.TenantID, func(q *db.Queries) error {
			return execChunkBatch(q.CreateChunks(ctx, rows), batchIdx)
		}); err != nil {
			return err
		}
	}
	return nil
}

// execChunkBatch drains a chunk-insert batch, reporting the first failure.
// Every queued statement must be drained even after one fails, or the
// connection is left with unread results.
func execChunkBatch(results *db.CreateChunksBatchResults, batchIdx int) error {
	var firstErr error
	results.Exec(func(i int, err error) {
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: batch %d, row %d: %w", batchIdx, i, err)
		}
	})
	if closeErr := results.Close(); closeErr != nil && firstErr == nil {
		firstErr = fmt.Errorf("store: batch %d: close: %w", batchIdx, closeErr)
	}
	return firstErr
}
