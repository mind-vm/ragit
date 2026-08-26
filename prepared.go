package ragit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit/embed"
)

// preparedInsertBatch bounds how many chunk rows go into one INSERT. Prepared
// chunks arrive with their vectors, so the whole write is one transaction —
// this only keeps a single statement from carrying a large document's entire
// corpus of vectors.
const preparedInsertBatch = 100

// PreparedChunk is one chunk of a document that was chunked and embedded
// outside ragit.
type PreparedChunk struct {
	// Content is the chunk's text, and is what a later run compares against
	// to decide whether the chunk can be reused. See [ResumeChunks].
	Content string
	// Embedding is the vector for Content, in the space named by
	// [PreparedDocument.Space]. Its length must match that space's Dimension.
	Embedding embed.Vector
	// HeadingPath is the chunk's heading trail, for citations. Optional, but
	// a citation UI has nothing to show without it.
	HeadingPath []string
	// Metadata is whatever the producing pipeline recorded about this chunk —
	// page spans, byte offsets, a table flag. Stored as-is; nil becomes {}.
	Metadata json.RawMessage
}

// PreparedDocument is a front half of the pipeline — extract, chunk, embed —
// that ragit did not run.
type PreparedDocument struct {
	// Text is the document's full extracted text, stored on the document row.
	Text string
	// Metadata is the extractor's document-level metadata. Nil becomes {}.
	Metadata json.RawMessage
	// Space identifies where Chunks' vectors live. Retrieval filters on its
	// fingerprint, so it must be the same space the query embedder reports.
	Space embed.Space
	// Chunks are the document's chunks in order: Chunks[i] is chunk index i.
	Chunks []PreparedChunk
}

func (d PreparedDocument) validate() error {
	if err := d.Space.Validate(); err != nil {
		return err
	}
	if d.Text == "" {
		return errors.New("ragit: PreparedDocument.Text is empty")
	}
	if len(d.Chunks) == 0 {
		return errors.New("ragit: PreparedDocument.Chunks is empty")
	}
	for i, c := range d.Chunks {
		switch {
		case c.Content == "":
			return fmt.Errorf("ragit: prepared chunk %d has no content", i)
		case len(c.Embedding) != d.Space.Dimension:
			return fmt.Errorf("ragit: prepared chunk %d has a %d-dimension vector, but Space declares %d",
				i, len(c.Embedding), d.Space.Dimension)
		case len(c.Metadata) > 0 && !json.Valid(c.Metadata):
			return fmt.Errorf("ragit: prepared chunk %d has invalid metadata JSON", i)
		}
	}
	if len(d.Metadata) > 0 && !json.Valid(d.Metadata) {
		return errors.New("ragit: PreparedDocument.Metadata is invalid JSON")
	}
	return nil
}

// IngestPrepared indexes a document whose extraction, chunking and embedding
// already happened somewhere else — an extraction service that returns chunks
// and vectors from one call, a batch job, another pipeline entirely.
//
// It is [Processor.ProcessDocument]'s sibling: same starting point (a document
// created by [Processor.CreateDocument]), same terminal states, same events,
// same resume guard — only the front half of the pipeline differs.
//
//	documentID, err := p.CreateDocument(ctx, in)
//	// ... run your own extract/chunk/embed ...
//	err = p.IngestPrepared(ctx, documentID, in.TenantID, prepared)
//
// This exists because the alternative is writing chunk rows by hand, and four
// of the things ragit does on that path fail *silently* when they are
// forgotten. IngestPrepared owns all four: the scope, attribute and expiry
// columns each chunk carries denormalized (miss one and searches quietly
// return the wrong rows, or none); the embedding fingerprint retrieval filters
// on; the document's terminal state; and the [EventSink] notification a host
// application's own catalog depends on.
//
// The whole write is one transaction — resume guard, chunk rows and terminal
// state together — because unlike ProcessDocument there is no provider call in
// the middle to keep it open. A prepared corpus is durable or it is absent,
// never half-written.
//
// It needs no extractor, chunker or embedder, so a Processor for this path can
// be built with nil for all three. A malformed PreparedDocument is reported
// without touching the document: a bad call is a caller's bug, not a failed
// document.
func (p *Processor) IngestPrepared(ctx context.Context, documentID, tenantID uuid.UUID, prepared PreparedDocument) error {
	if err := prepared.validate(); err != nil {
		return err
	}

	doc, err := p.beginProcessing(ctx, documentID, tenantID)
	if err != nil {
		return err
	}
	return p.terminal(ctx, doc, p.ingestPrepared(ctx, doc, prepared))
}

func (p *Processor) ingestPrepared(ctx context.Context, doc *Document, prepared PreparedDocument) error {
	if p.maxChunksPerDoc > 0 && len(prepared.Chunks) > p.maxChunksPerDoc {
		msg := fmt.Sprintf("document arrived with %d chunks, exceeding the %d-chunk limit; indexing skipped",
			len(prepared.Chunks), p.maxChunksPerDoc)
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

	fingerprint := prepared.Space.Fingerprint()
	contents := make([]string, len(prepared.Chunks))
	for i, c := range prepared.Chunks {
		contents[i] = c.Content
	}

	if err := WithTenant(ctx, p.pool, doc.TenantID, func(db sqlb.Executor) error {
		reusable, err := ResumeChunks(ctx, db, doc.TenantID, doc.ID, contents, fingerprint)
		if err != nil {
			return err
		}

		pending := make([]*Chunk, 0, len(prepared.Chunks))
		for i, c := range prepared.Chunks {
			if reusable[i] {
				continue
			}
			pending = append(pending, preparedRow(doc, i, c, fingerprint))
		}

		for start := 0; start < len(pending); start += preparedInsertBatch {
			end := min(start+preparedInsertBatch, len(pending))
			if _, err := sqlb.InsertRows(pending[start:end]...).
				Omit("id", "created_at").
				Exec(ctx, db); err != nil {
				return fmt.Errorf("ragit: store prepared chunks: %w", err)
			}
		}
		return nil
	}); err != nil {
		return err
	}

	count := int32(len(prepared.Chunks))
	done := &indexed{text: prepared.Text, metadata: prepared.Metadata, model: prepared.Space.Model}
	if err := p.finish(ctx, doc, StatusReady, nil, &count, done); err != nil {
		return err
	}
	p.publish(ctx, doc, StatusReady, "")
	return nil
}

// preparedRow denormalizes the document's confinement onto a chunk. Every
// column copied here is one a search reads instead of joining for, and one a
// hand-written writer can silently omit.
func preparedRow(doc *Document, index int, c PreparedChunk, fingerprint string) *Chunk {
	vec := sqlb.Vector(c.Embedding)
	metadata := c.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage("{}")
	}
	return &Chunk{
		DocumentID:           doc.ID,
		TenantID:             doc.TenantID,
		ScopeAID:             doc.ScopeAID,
		ScopeBID:             doc.ScopeBID,
		SessionID:            doc.SessionID,
		ChunkIndex:           int32(index),
		HeadingPath:          c.HeadingPath,
		Content:              c.Content,
		Embedding:            &vec,
		EmbeddingFingerprint: &fingerprint,
		Metadata:             metadata,
		Attributes:           doc.Attributes,
		ExpiresAt:            doc.ExpiresAt,
	}
}
