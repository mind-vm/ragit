// Package ragit is a reusable RAG pipeline: extract, chunk, embed, and store a
// document, then retrieve it. See docs/design.md for the full design and the
// production reference implementation it's grounded in.
//
// Four properties are worth knowing before wiring this in.
//
// [Processor.ProcessDocument] is resumable. An interrupted run picks up from
// whatever was already embedded in the current embedding space rather than
// re-billing every chunk on retry — see the jobs package for running it under
// River.
//
// Every read is confined by a [Scope], whose zero value matches no rows. A
// retrieval or catalog call that forgets its confinement returns [ErrUnscoped]
// rather than another tenant's documents.
//
// Beneath that, the tables carry FORCE ROW LEVEL SECURITY and every query runs
// inside a tenant-scoped transaction, so isolation is enforced by the database
// as well as by the query — but only if the application connects as a
// non-superuser role, since PostgreSQL exempts superusers from RLS. See
// [NewPool] and [WithTenant].
//
// The schema is declared in ragitschema and the models here are generated from
// it. They are exported deliberately: a consumer that needs a read ragit does
// not offer can write it with sqlb against [Document] and [Chunk] rather than
// being blocked by an internal package.
package ragit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb"

	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/extract"
	"github.com/jryannel/ragit/store"
)

// Document statuses.
const (
	StatusPending         = "pending"
	StatusProcessing      = "processing"
	StatusReady           = "ready"
	StatusError           = "error"
	StatusSkippedTooLarge = "skipped_too_large"
)

// ErrNotFound is returned when a document does not exist, or is not visible to
// the scope that asked for it. The two are deliberately indistinguishable:
// telling a caller that a document exists but belongs to someone else is
// itself a disclosure.
var ErrNotFound = errors.New("ragit: document not found")

// DocumentInput describes a document to ingest.
type DocumentInput struct {
	// TenantID is required; it is the security boundary.
	TenantID uuid.UUID
	// ScopeA and ScopeB file the document under ragit's two generic scope
	// dimensions. ragit does not know what they mean — a host application maps
	// its own domain onto them, and searches confine with the matching
	// [Scope].
	ScopeA *uuid.UUID
	ScopeB *uuid.UUID
	// SessionID marks the document as an ephemeral attachment belonging to one
	// conversation or agent session. Such documents are invisible to ordinary
	// library search unless a caller names that session.
	SessionID *uuid.UUID
	// Attributes are the application's own key/value pairs, stored on the
	// document and denormalized onto its chunks so searches can filter by
	// them. They narrow a result set; they do not confine it — see
	// [Attributes].
	Attributes Attributes
	// ExpiresAt sets a retention clock on the document and its chunks. Nil
	// keeps it until explicitly deleted.
	ExpiresAt *time.Time
	Filename  string
	MimeType  string
	Data      []byte
}

func (in DocumentInput) validate() error {
	switch {
	case in.TenantID == uuid.Nil:
		return fmt.Errorf("%w: DocumentInput.TenantID is required", ErrUnscoped)
	case in.Filename == "":
		return errors.New("ragit: DocumentInput.Filename is required")
	case len(in.Data) == 0:
		return errors.New("ragit: DocumentInput.Data is empty")
	}
	return nil
}

// Processor wires extraction, chunking, embedding, storage, and retrieval into
// one pipeline.
type Processor struct {
	pool      *pgxpool.Pool
	extractor extract.Extractor
	chunker   *chunk.Chunker
	embedder  embed.Embedder
	store     store.Store

	maxChunksPerDoc int
	sink            EventSink
}

// New builds a Processor. The caller owns pool/store's lifecycle.
func New(pool *pgxpool.Pool, extractor extract.Extractor, chunker *chunk.Chunker, embedder embed.Embedder, st store.Store) *Processor {
	return &Processor{
		pool:      pool,
		extractor: extractor,
		chunker:   chunker,
		embedder:  embedder,
		store:     st,
	}
}

// WithMaxChunksPerDocument sets the per-document chunk cap (0 = no cap) and
// returns the Processor for chaining. Above the cap, embedding is skipped and
// the document is flagged skipped_too_large instead of consuming the embedding
// budget.
func (p *Processor) WithMaxChunksPerDocument(n int) *Processor {
	p.maxChunksPerDoc = n
	return p
}

// WithEventSink attaches a sink notified when a document reaches a terminal
// state. See [EventSink] for what is and is not guaranteed.
func (p *Processor) WithEventSink(sink EventSink) *Processor {
	p.sink = sink
	return p
}

// embedBatchSize bounds how many chunks are embedded per provider call.
const embedBatchSize = 10

// nowFunc is time.Now, named so the mutation helpers read the same way.
var nowFunc = time.Now

// CreateDocument stores the bytes and inserts a pending row. Fast and
// synchronous — meant to be called from an upload handler, before a
// ProcessDocument job is enqueued.
func (p *Processor) CreateDocument(ctx context.Context, in DocumentInput) (uuid.UUID, error) {
	if err := in.validate(); err != nil {
		return uuid.Nil, err
	}

	attributes, err := in.Attributes.raw()
	if err != nil {
		return uuid.Nil, err
	}

	uri, err := p.store.Put(ctx, in.TenantID, in.Filename, in.Data, in.MimeType)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ragit: store document: %w", err)
	}

	row := &Document{
		TenantID:   in.TenantID,
		ScopeAID:   in.ScopeA,
		ScopeBID:   in.ScopeB,
		SessionID:  in.SessionID,
		SourceURI:  &uri,
		Filename:   in.Filename,
		MimeType:   in.MimeType,
		Status:     StatusPending,
		Metadata:   json.RawMessage("{}"),
		Attributes: attributes,
		ExpiresAt:  in.ExpiresAt,
	}

	var id uuid.UUID
	err = WithTenant(ctx, p.pool, in.TenantID, func(db sqlb.Executor) error {
		created, err := sqlb.InsertRows(row).
			Omit("id", "created_at", "updated_at", "status", "metadata").
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: create document row: %w", err)
		}
		id = created.ID
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// ProcessDocument runs extract→chunk→embed→store for an existing document,
// resuming from whatever was already embedded in the current embedding space
// rather than re-billing chunks a prior attempt already paid for.
//
// The document always ends in status ready, error, or skipped_too_large.
// ProcessDocument still returns the underlying error on failure — callers need
// it to decide whether the failure is worth retrying; it is not swallowed into
// a nil-error result.
//
// The work is split across several short transactions rather than held in one.
// That is deliberate: a single transaction spanning the extractor's and
// embedding provider's HTTP calls would hold a connection open for the whole
// run and — worse — make the per-batch checkpointing meaningless, since
// nothing would be durable until the final commit.
func (p *Processor) ProcessDocument(ctx context.Context, documentID, tenantID uuid.UUID) error {
	var doc Document
	err := WithTenant(ctx, p.pool, tenantID, func(db sqlb.Executor) error {
		found, err := sqlb.Query[Document]().
			Where(DocumentCols.ID.Eq(documentID), DocumentCols.TenantID.Eq(tenantID)).
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNotFound, documentID)
		}
		doc = found

		_, err = sqlb.UpdateRows[Document]().
			Set("status", StatusProcessing).
			Set("updated_at", time.Now()).
			Where(DocumentCols.ID.Eq(documentID)).
			Exec(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: mark document processing: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	processErr := p.processDocument(ctx, &doc)
	if processErr != nil {
		msg := processErr.Error()
		if updateErr := p.finish(ctx, &doc, StatusError, &msg, nil, nil); updateErr != nil {
			return fmt.Errorf("ragit: mark document error (after %v): %w", processErr, updateErr)
		}
		p.publish(ctx, &doc, StatusError, msg)
		return processErr
	}
	return nil
}

func (p *Processor) processDocument(ctx context.Context, doc *Document) error {
	if doc.SourceURI == nil {
		return errors.New("ragit: document has no stored source")
	}
	reader, err := p.store.Get(ctx, *doc.SourceURI)
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
		msg := fmt.Sprintf("document produced %d chunks, exceeding the %d-chunk limit; embedding skipped",
			len(chunks), p.maxChunksPerDoc)
		if err := p.clearChunks(ctx, doc); err != nil {
			return err
		}
		zero := int32(0)
		if err := p.finish(ctx, doc, StatusSkippedTooLarge, &msg, &zero, nil); err != nil {
			return err
		}
		p.publish(ctx, doc, StatusSkippedTooLarge, msg)
		return nil
	}

	if err := p.embedAndStore(ctx, doc, chunks); err != nil {
		return err
	}

	count := int32(len(chunks))
	if err := p.finish(ctx, doc, StatusReady, nil, &count, result); err != nil {
		return err
	}
	p.publish(ctx, doc, StatusReady, "")
	return nil
}

// finish writes the document's terminal state.
func (p *Processor) finish(ctx context.Context, doc *Document, status string, errMsg *string, chunkCount *int32, result *extract.Result) error {
	return WithTenant(ctx, p.pool, doc.TenantID, func(db sqlb.Executor) error {
		u := sqlb.UpdateRows[Document]().
			Set("status", status).
			Set("error", errMsg).
			Set("updated_at", time.Now()).
			Where(DocumentCols.ID.Eq(doc.ID))

		if chunkCount != nil {
			u = u.Set("chunk_count", *chunkCount)
		}
		if result != nil {
			metadata := result.Metadata
			if len(metadata) == 0 {
				metadata = json.RawMessage("{}")
			}
			model := p.embedder.Model()
			now := time.Now()
			u = u.Set("text_content", &result.Text).
				Set("metadata", metadata).
				Set("embedding_model", &model).
				Set("processed_at", &now)
		}

		if _, err := u.Exec(ctx, db); err != nil {
			return fmt.Errorf("ragit: mark document %s: %w", status, err)
		}
		return nil
	})
}

// Ingest is a synchronous convenience wrapper around CreateDocument +
// ProcessDocument, for callers that don't need async job processing. On
// failure it still returns the underlying error; the Document reflects the
// persisted state either way.
func (p *Processor) Ingest(ctx context.Context, in DocumentInput) (*Document, error) {
	documentID, err := p.CreateDocument(ctx, in)
	if err != nil {
		return nil, err
	}

	processErr := p.ProcessDocument(ctx, documentID, in.TenantID)

	doc, err := p.GetDocument(ctx, Tenant(in.TenantID).AnyA().AnyB().sessionOf(in.SessionID), documentID)
	if err != nil {
		return nil, fmt.Errorf("ragit: load document after processing: %w", err)
	}
	return doc, processErr
}

// sessionOf widens a scope to a session when the document has one, so Ingest
// can read back an ephemeral attachment it just created.
func (s Scope) sessionOf(sessionID *uuid.UUID) Scope {
	if sessionID == nil {
		return s
	}
	return s.Session(*sessionID)
}

// MoveDocumentScope reassigns a document's scope dimensions and re-stamps its
// chunks to match.
//
// The resync is why this method exists rather than callers updating the row
// themselves: chunks carry denormalized copies of the scope columns so that
// retrieval never needs a join, and those copies do not self-heal.
// Reprocessing does not fix them either — the resume check sees identical
// content, skips the rewrite, and leaves the chunks answering searches for
// their old scope.
func (p *Processor) MoveDocumentScope(ctx context.Context, tenantID, documentID uuid.UUID, scopeA, scopeB, sessionID *uuid.UUID) error {
	return WithTenant(ctx, p.pool, tenantID, func(db sqlb.Executor) error {
		rows, err := sqlb.UpdateRows[Document]().
			Set("scope_a_id", scopeA).
			Set("scope_b_id", scopeB).
			Set("session_id", sessionID).
			Set("updated_at", time.Now()).
			Where(DocumentCols.ID.Eq(documentID), DocumentCols.TenantID.Eq(tenantID)).
			Exec(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: update document scope: %w", err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, documentID)
		}

		if _, err := sqlb.UpdateRows[Chunk]().
			Set("scope_a_id", scopeA).
			Set("scope_b_id", scopeB).
			Set("session_id", sessionID).
			Where(ChunkCols.DocumentID.Eq(documentID), ChunkCols.TenantID.Eq(tenantID)).
			Exec(ctx, db); err != nil {
			return fmt.Errorf("ragit: resync chunk scope: %w", err)
		}
		return nil
	})
}

// DeleteDocument removes a document, its chunks (cascaded via the FK), and the
// original bytes in object storage.
//
// The database row goes first. If the object-storage delete then fails, the
// result is an orphaned object rather than a document that still answers
// searches but whose bytes have vanished — the cheaper of the two
// inconsistencies, and the one a storage lifecycle rule can mop up. The error
// is still returned so the caller knows it happened.
func (p *Processor) DeleteDocument(ctx context.Context, tenantID, documentID uuid.UUID) error {
	var sourceURI *string
	if err := WithTenant(ctx, p.pool, tenantID, func(db sqlb.Executor) error {
		doc, err := sqlb.Query[Document]().
			Where(DocumentCols.ID.Eq(documentID), DocumentCols.TenantID.Eq(tenantID)).
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNotFound, documentID)
		}
		sourceURI = doc.SourceURI

		if _, err := sqlb.DeleteRows[Document]().
			Where(DocumentCols.ID.Eq(documentID), DocumentCols.TenantID.Eq(tenantID)).
			Exec(ctx, db); err != nil {
			return fmt.Errorf("ragit: delete document: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	if sourceURI != nil {
		if err := p.store.Delete(ctx, *sourceURI); err != nil {
			return fmt.Errorf("ragit: purge stored document: %w", err)
		}
	}
	return nil
}

func (p *Processor) clearChunks(ctx context.Context, doc *Document) error {
	return WithTenant(ctx, p.pool, doc.TenantID, func(db sqlb.Executor) error {
		if _, err := sqlb.DeleteRows[Chunk]().
			Where(ChunkCols.DocumentID.Eq(doc.ID), ChunkCols.TenantID.Eq(doc.TenantID)).
			Exec(ctx, db); err != nil {
			return fmt.Errorf("ragit: clear chunks: %w", err)
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
		return nil, nil, errors.New("extract: no text content extracted")
	}

	chunks := p.chunker.SplitMarkdown(result.Text)
	if len(chunks) == 0 {
		return nil, nil, errors.New("chunk: no chunks produced")
	}
	return result, chunks, nil
}

// embedAndStore embeds chunks not already embedded in the current embedding
// space and persists each batch as it completes, in its own transaction, so an
// interrupted run keeps everything it already paid for.
//
// A persisted chunk is reused only if its fingerprint matches the live
// embedder AND its content matches the freshly re-chunked text at that index;
// any mismatch wipes every chunk for the document and starts clean, so
// embedding spaces never mix within one document.
func (p *Processor) embedAndStore(ctx context.Context, doc *Document, chunks []chunk.Chunk) error {
	currentFP := embed.Fingerprint(p.embedder)

	fresh := make(map[int32]string, len(chunks))
	for _, c := range chunks {
		fresh[int32(c.Index)] = c.Content
	}

	embedded := make(map[int32]bool, len(chunks))
	if err := WithTenant(ctx, p.pool, doc.TenantID, func(db sqlb.Executor) error {
		existing, err := sqlb.Query[Chunk]().
			Where(ChunkCols.DocumentID.Eq(doc.ID), ChunkCols.TenantID.Eq(doc.TenantID)).
			OrderBy(ChunkCols.ChunkIndex.Asc()).
			All(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: load existing chunks: %w", err)
		}

		stale := false
		for _, e := range existing {
			if e.EmbeddingFingerprint != nil && *e.EmbeddingFingerprint == currentFP && fresh[e.ChunkIndex] == e.Content {
				embedded[e.ChunkIndex] = true
			} else {
				stale = true
				break
			}
		}
		if stale {
			if _, err := sqlb.DeleteRows[Chunk]().
				Where(ChunkCols.DocumentID.Eq(doc.ID), ChunkCols.TenantID.Eq(doc.TenantID)).
				Exec(ctx, db); err != nil {
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
			return fmt.Errorf("embed: batch %d: got %d vectors for %d chunks",
				start/embedBatchSize, len(vectors), len(pending))
		}

		rows := make([]*Chunk, len(pending))
		for i, c := range pending {
			vec := sqlb.Vector(vectors[i])
			rows[i] = &Chunk{
				DocumentID:           doc.ID,
				TenantID:             doc.TenantID,
				ScopeAID:             doc.ScopeAID,
				ScopeBID:             doc.ScopeBID,
				SessionID:            doc.SessionID,
				ChunkIndex:           int32(c.Index),
				HeadingPath:          c.HeadingPath,
				Content:              c.Content,
				Embedding:            &vec,
				EmbeddingFingerprint: &currentFP,
				Metadata:             json.RawMessage("{}"),
				Attributes:           doc.Attributes,
				ExpiresAt:            doc.ExpiresAt,
			}
		}

		batchIdx := start / embedBatchSize
		if err := WithTenant(ctx, p.pool, doc.TenantID, func(db sqlb.Executor) error {
			if _, err := sqlb.InsertRows(rows...).
				Omit("id", "created_at", "metadata").
				Exec(ctx, db); err != nil {
				return fmt.Errorf("store: batch %d: %w", batchIdx, err)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}
