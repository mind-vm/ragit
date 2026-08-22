// Package search retrieves chunks for a query.
//
// Vector search and full-text search are two separate calls, not a single
// fused "hybrid" endpoint. Fusing them (reciprocal rank fusion or similar)
// means committing to one blend of the two rankings for every caller; the
// blend that suits a citation UI is rarely the one that suits an agent's
// tool call. Callers that want fusion can run both and combine the results
// themselves, and a fused entry point can be added later once there is real
// query data to tune it against.
package search

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/internal/db"
)

// DefaultTopK is used when Options.TopK is left at zero.
const DefaultTopK = 10

// Result is one retrieved chunk, carrying enough context to cite it.
type Result struct {
	ChunkID    uuid.UUID
	DocumentID uuid.UUID
	Filename   string
	ChunkIndex int
	// HeadingPath is the chunk's trail of Markdown headings, e.g.
	// {"Chapter 2", "Section 2.1"} — the raw material for a citation.
	HeadingPath []string
	Content     string
	Metadata    json.RawMessage
	// Score is cosine similarity (1 = identical) for vector search, and a
	// ts_rank value for full-text search. The two are not comparable, and
	// neither has a meaningful absolute scale across models — see MinScore.
	Score float64
}

// Options narrows a search. The zero value is the most restrictive useful
// setting rather than the widest: a caller that forgets to set a field
// under-fetches instead of leaking rows it should not have returned.
type Options struct {
	// TopK caps the number of results. Zero means DefaultTopK.
	TopK int

	// MinScore drops results below a cosine-similarity cutoff. It applies to
	// vector search only, and there is deliberately no default beyond zero:
	// the band that separates a relevant match from noise is a property of
	// the embedding model, not of retrieval in general (Gemini's relevant
	// matches sit around 0.5–0.7, OpenAI's much higher), so a value baked in
	// here would be wrong for most models. Calibrate it per embedder.
	MinScore float64

	// ScopeID restricts results to one scope. Nil searches every scope
	// within the tenant. Note that the nested-scope cascade described in
	// design.md §8 — querying a project and also getting its work-packages,
	// or always including org-level documents — is NOT implemented: this is
	// an exact match on a reserved column. Cascade semantics need a real
	// product requirement to pin down before they are worth building.
	ScopeID *uuid.UUID

	// SessionID opts a single ephemeral session's chunks into the results,
	// alongside the durable library. Nil — the zero value — excludes every
	// session-scoped chunk, which is why this is safe to leave unset: an
	// attachment uploaded into one conversation does not surface in another
	// user's library search just because a caller forgot a filter.
	SessionID *uuid.UUID
}

func (o Options) limit() int32 {
	if o.TopK <= 0 {
		return DefaultTopK
	}
	return int32(o.TopK)
}

// Searcher runs retrieval queries against the chunk store.
type Searcher struct {
	pool     *pgxpool.Pool
	embedder embed.Embedder
}

// New builds a Searcher. The embedder must be the same one that produced the
// stored vectors; Vector filters on its fingerprint precisely so that a
// mismatch returns nothing rather than nonsense.
func New(pool *pgxpool.Pool, embedder embed.Embedder) *Searcher {
	return &Searcher{pool: pool, embedder: embedder}
}

// Vector embeds the query and returns the nearest chunks by cosine
// similarity.
//
// Only chunks embedded by the active embedder are considered. Cosine
// distance between vectors from different models is not a weaker signal, it
// is a meaningless one, so chunks from another embedding space are excluded
// rather than ranked. If a provider or model changed without the corpus
// being re-embedded, this returns fewer results (or none) instead of
// confidently wrong ones — use [Searcher.CountMisalignedChunks] to detect
// that state deliberately.
func (s *Searcher) Vector(ctx context.Context, tenantID uuid.UUID, query string, opts Options) ([]Result, error) {
	if query == "" {
		return nil, fmt.Errorf("ragit/search: empty query")
	}

	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("ragit/search: embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("ragit/search: embedder returned %d vectors for one query", len(vectors))
	}

	params := db.SearchChunksByVectorParams{
		QueryEmbedding:       pgvector.NewVector(vectors[0]),
		TenantID:             tenantID,
		EmbeddingFingerprint: embed.Fingerprint(s.embedder),
		ScopeID:              opts.ScopeID,
		SessionID:            opts.SessionID,
		MinScore:             opts.MinScore,
		ResultLimit:          opts.limit(),
	}

	var results []Result
	err = db.WithTenant(ctx, s.pool, tenantID, func(q *db.Queries) error {
		rows, err := q.SearchChunksByVector(ctx, params)
		if err != nil {
			return fmt.Errorf("ragit/search: vector query: %w", err)
		}
		results = make([]Result, len(rows))
		for i, r := range rows {
			results[i] = Result{
				ChunkID:     r.ID,
				DocumentID:  r.DocumentID,
				Filename:    r.Filename,
				ChunkIndex:  int(r.ChunkIndex),
				HeadingPath: r.HeadingPath,
				Content:     r.Content,
				Metadata:    r.Metadata,
				Score:       r.Score,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// FullText runs a Postgres full-text query, ranked by ts_rank.
//
// The query goes through websearch_to_tsquery, so callers can pass what a
// user typed — quoted phrases, OR, leading minus — without sanitising it
// into tsquery syntax, and without malformed input raising an error the way
// to_tsquery would.
func (s *Searcher) FullText(ctx context.Context, tenantID uuid.UUID, query string, opts Options) ([]Result, error) {
	if query == "" {
		return nil, fmt.Errorf("ragit/search: empty query")
	}

	params := db.SearchChunksByTextParams{
		Query:       query,
		TenantID:    tenantID,
		ScopeID:     opts.ScopeID,
		SessionID:   opts.SessionID,
		ResultLimit: opts.limit(),
	}

	var results []Result
	err := db.WithTenant(ctx, s.pool, tenantID, func(q *db.Queries) error {
		rows, err := q.SearchChunksByText(ctx, params)
		if err != nil {
			return fmt.Errorf("ragit/search: full-text query: %w", err)
		}
		results = make([]Result, len(rows))
		for i, r := range rows {
			results[i] = Result{
				ChunkID:     r.ID,
				DocumentID:  r.DocumentID,
				Filename:    r.Filename,
				ChunkIndex:  int(r.ChunkIndex),
				HeadingPath: r.HeadingPath,
				Content:     r.Content,
				Metadata:    r.Metadata,
				Score:       r.Score,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// CountMisalignedChunks reports how many of a tenant's embedded chunks were
// produced by an embedder other than the active one.
//
// A non-zero count means the corpus straddles two embedding spaces, and that
// [Searcher.Vector] is silently ignoring part of it. Call this at startup and
// decide what it means for your deployment — block queries, or log loudly and
// schedule a re-embed. It is reported rather than acted on here because
// "refuse to serve" and "serve a degraded corpus" are both defensible, and
// which one is right is the host application's call.
func (s *Searcher) CountMisalignedChunks(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	var count int64
	err := db.WithTenant(ctx, s.pool, tenantID, func(q *db.Queries) error {
		var err error
		count, err = q.CountChunksWithForeignFingerprint(ctx, db.CountChunksWithForeignFingerprintParams{
			TenantID:             tenantID,
			EmbeddingFingerprint: embed.Fingerprint(s.embedder),
		})
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("ragit/search: count misaligned chunks: %w", err)
	}
	return count, nil
}
