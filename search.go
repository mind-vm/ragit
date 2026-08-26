package ragit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit/embed"
)

// DefaultTopK is used when SearchOptions.TopK is left at zero.
const DefaultTopK = 10

// SearchResult is one retrieved chunk, carrying enough context to cite it.
type SearchResult struct {
	ChunkID    uuid.UUID
	DocumentID uuid.UUID
	Filename   string
	ChunkIndex int32
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

// citation is the shared projection both searches scan into. The score column
// is aliased "score" in each, even though one is a cosine similarity and the
// other a ts_rank: the alias names the slot, and SearchResult.Score documents
// that what fills it differs by query.
type citation struct {
	ChunkID     uuid.UUID       `db:"id"`
	DocumentID  uuid.UUID       `db:"document_id"`
	ChunkIndex  int32           `db:"chunk_index"`
	HeadingPath []string        `db:"heading_path"`
	Content     string          `db:"content"`
	Metadata    json.RawMessage `db:"metadata"`
	Filename    string          `db:"filename"`
	Score       float64         `db:"score"`
}

func (c citation) result() SearchResult {
	return SearchResult{
		ChunkID:     c.ChunkID,
		DocumentID:  c.DocumentID,
		Filename:    c.Filename,
		ChunkIndex:  c.ChunkIndex,
		HeadingPath: c.HeadingPath,
		Content:     c.Content,
		Metadata:    c.Metadata,
		Score:       c.Score,
	}
}

// SearchOptions tunes a search. Confinement is not here: it is the [Scope]
// argument, which is required and cannot be defaulted away.
type SearchOptions struct {
	// TopK caps the number of results. Zero means DefaultTopK.
	TopK int

	// MinScore drops results below a cosine-similarity cutoff. It applies to
	// vector search only, and there is deliberately no default beyond zero:
	// the band separating a relevant match from noise is a property of the
	// embedding model, not of retrieval in general (Gemini's relevant matches
	// sit around 0.5–0.7, OpenAI's much higher), so a value baked in here
	// would be wrong for most models. Calibrate it per embedder.
	MinScore float64

	// Attributes narrows to chunks whose document carries all of these
	// key/value pairs. Empty narrows nothing — this filters a result set that
	// Scope has already confined, and is not itself a boundary. See
	// [Attributes].
	Attributes Attributes
}

// preds renders the options' own predicates, which narrow rather than confine.
//
// The column is qualified: both tables in the join carry `attributes`, and the
// one to filter is the chunk's denormalized copy — that is what the chunk GIN
// index covers, and filtering the joined document instead would fight the HNSW
// scan rather than ride alongside it.
func (o SearchOptions) preds() ([]sqlb.Pred, error) {
	pred, ok, err := o.Attributes.containsPred(`"ragit_chunks"."attributes"`)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	return []sqlb.Pred{pred}, nil
}

func (o SearchOptions) limit() int {
	if o.TopK <= 0 {
		return DefaultTopK
	}
	return o.TopK
}

// VectorSearch returns the chunks nearest to query by cosine similarity.
//
// Only chunks embedded by the active embedder are considered. Cosine distance
// between vectors from different models is not a weaker signal, it is a
// meaningless one, so chunks from another embedding space are excluded rather
// than ranked. If a provider or model changed without the corpus being
// re-embedded, this returns fewer results (or none) instead of confidently
// wrong ones — use [Processor.CountMisalignedChunks] to detect that state
// deliberately.
func (p *Processor) VectorSearch(ctx context.Context, scope Scope, query string, opts SearchOptions) ([]SearchResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if query == "" {
		return nil, errors.New("ragit: empty query")
	}
	if p.embedder == nil {
		return nil, errNoEmbedder
	}

	vectors, err := p.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("ragit: embed query: %w", err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("ragit: embedder returned %d vectors for one query", len(vectors))
	}

	near := sqlb.Near(sqlb.F("embedding"), sqlb.Vector(vectors[0]))
	fingerprint := embed.Fingerprint(p.embedder)

	narrowing, err := opts.preds()
	if err != nil {
		return nil, err
	}

	preds := append(scope.preds(),
		sqlb.F("embedding").NotNull(),
		sqlb.F("embedding_fingerprint").Eq(fingerprint),
	)
	preds = append(preds, narrowing...)
	if opts.MinScore > 0 {
		preds = append(preds, near.AtLeast(opts.MinScore))
	}

	q := sqlb.Query[Chunk]().
		Select(citationColumns(near.Similarity().As("score"))...).
		Join("ragit_documents", "d", sqlb.F("document_id").EqField(sqlb.F("d.id"))).
		Where(preds...).
		OrderBy(near.Nearest()).
		Limit(opts.limit())

	return collectResults(ctx, p.pool, scope, q)
}

// FullTextSearch returns chunks matching query via Postgres full-text search,
// ranked by ts_rank.
//
// It is a separate call from [Processor.VectorSearch] rather than fused with
// it. Fusing the two (reciprocal rank fusion or similar) means committing to
// one blend of the rankings for every caller, and the blend that suits a
// citation UI is rarely the one that suits an agent's tool call. A caller who
// wants fusion can run both and combine them.
//
// The query goes through websearch_to_tsquery, so a caller can pass what a
// user typed — quoted phrases, OR, leading minus — without sanitising it into
// tsquery syntax, and without malformed input raising an error the way
// to_tsquery would.
func (p *Processor) FullTextSearch(ctx context.Context, scope Scope, query string, opts SearchOptions) ([]SearchResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if query == "" {
		return nil, errors.New("ragit: empty query")
	}

	// The 'simple' configuration must match the one the search_vector column
	// was generated with. An 'english' column stores stemmed lexemes, and
	// matching those against unstemmed query terms under-matches silently.
	//
	// This is sqlb.Raw rather than a builder predicate because sqlb's own
	// Searchable capability compiles to ILIKE '%…%', which is a different
	// feature: it is not ranked, and it cannot express websearch_to_tsquery's
	// grammar.
	tsquery := "websearch_to_tsquery('simple', ?)"
	rank := sqlb.Raw{SQL: "ts_rank(search_vector, " + tsquery + ")", Args: []any{query}}
	matches := sqlb.RawPred("search_vector @@ "+tsquery, query)

	narrowing, err := opts.preds()
	if err != nil {
		return nil, err
	}

	q := sqlb.Query[Chunk]().
		Select(citationColumns(sqlb.Sel(rank).As("score"))...).
		Join("ragit_documents", "d", sqlb.F("document_id").EqField(sqlb.F("d.id"))).
		Where(append(append(scope.preds(), matches), narrowing...)...).
		OrderBy(sqlb.OrderByDesc(rank), sqlb.F("document_id").Asc(), sqlb.F("chunk_index").Asc()).
		Limit(opts.limit())

	return collectResults(ctx, p.pool, scope, q)
}

// citationColumns is the projection both searches share: enough of the chunk
// to cite it, plus the document's filename from the join, plus the score.
func citationColumns(score sqlb.Selectable) []sqlb.Selectable {
	return []sqlb.Selectable{
		sqlb.F("id"),
		sqlb.F("document_id"),
		sqlb.F("chunk_index"),
		sqlb.F("heading_path"),
		sqlb.F("content"),
		sqlb.F("metadata"),
		sqlb.Sel(sqlb.F("d.filename").Column()).As("filename"),
		score,
	}
}

func collectResults(ctx context.Context, pool *pgxpool.Pool, scope Scope, q *sqlb.Builder[Chunk]) ([]SearchResult, error) {
	var out []SearchResult
	err := WithTenant(ctx, pool, scope.TenantID(), func(db sqlb.Executor) error {
		rows, err := sqlb.Collect[citation](ctx, db, q)
		if err != nil {
			return fmt.Errorf("ragit: search: %w", err)
		}
		out = make([]SearchResult, len(rows))
		for i, r := range rows {
			out[i] = r.result()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountMisalignedChunks reports how many of a tenant's embedded chunks were
// produced by an embedder other than the active one.
//
// A non-zero count means the corpus straddles two embedding spaces and that
// [Processor.VectorSearch] is silently ignoring part of it. Call it at startup
// and decide what it means for your deployment — block queries, or log loudly
// and schedule a re-embed. It is reported rather than acted on because "refuse
// to serve" and "serve a degraded corpus" are both defensible, and which is
// right is the host application's call.
func (p *Processor) CountMisalignedChunks(ctx context.Context, scope Scope) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}
	if p.embedder == nil {
		return 0, errNoEmbedder
	}
	fingerprint := embed.Fingerprint(p.embedder)

	var count int64
	err := WithTenant(ctx, p.pool, scope.TenantID(), func(db sqlb.Executor) error {
		n, err := sqlb.Query[Chunk]().
			Where(
				ChunkCols.TenantID.Eq(scope.TenantID()),
				sqlb.F("embedding").NotNull(),
				sqlb.RawPred("embedding_fingerprint IS DISTINCT FROM ?", fingerprint),
			).
			Count(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: count misaligned chunks: %w", err)
		}
		count = n
		return nil
	})
	return count, err
}
