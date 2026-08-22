package ragit_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/extract"
	"github.com/jryannel/ragit/internal/db"
	"github.com/jryannel/ragit/internal/testutil"
	"github.com/jryannel/ragit/search"
	"github.com/jryannel/ragit/store"
)

const searchEmbedDim = 1536

// searchKeywords assigns each keyword its own dimension in the mock embedding
// space, so a query containing one keyword lands at cosine similarity 1.0
// with documents containing it and 0.0 with the others. That makes ranking
// assertions exact rather than approximate.
var searchKeywords = []string{"postgres", "kubernetes", "tesseract"}

// newEchoExtractServer returns the uploaded bytes back as Markdown, so a test
// can ingest arbitrary text through one server rather than standing up a
// fixture-specific mock per document.
func newEchoExtractServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseMultipartForm(1<<20))
		f, _, err := r.FormFile("files")
		require.NoError(t, err)
		defer func() { _ = f.Close() }()
		content, err := io.ReadAll(f)
		require.NoError(t, err)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"content": string(content), "mime_type": "text/markdown"}},
			"errors":  []any{},
		})
	}))
}

// newKeywordEmbedServer maps text to a one-hot-ish vector over searchKeywords.
func newKeywordEmbedServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		data := make([]map[string]any, len(req.Input))
		for i, text := range req.Input {
			vec := make([]float32, searchEmbedDim)
			lower := strings.ToLower(text)
			matched := false
			for k, kw := range searchKeywords {
				if strings.Contains(lower, kw) {
					vec[k] = 1
					matched = true
				}
			}
			if !matched {
				// Never emit a zero vector: cosine distance against it is
				// undefined and pgvector would return NaN scores.
				vec[searchEmbedDim-1] = 1
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

// searchHarness is a processor wired to the echo extractor and keyword
// embedder, with a chunk size large enough that each small fixture document
// becomes exactly one chunk.
type searchHarness struct {
	processor *ragit.Processor
	pool      *pgxpool.Pool
	embedder  embed.Embedder
}

func newSearchHarness(t *testing.T, provider string) *searchHarness {
	t.Helper()
	pool := testutil.SetupTestPool(t)

	extractServer := newEchoExtractServer(t)
	t.Cleanup(extractServer.Close)
	embedServer := newKeywordEmbedServer(t)
	t.Cleanup(embedServer.Close)

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey:    "k",
		BaseURL:   embedServer.URL,
		Provider:  provider,
		Dimension: searchEmbedDim,
	})
	require.NoError(t, err)

	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 1000, Overlap: 0}),
		embedder,
		store.NewMemoryStore(),
	)
	return &searchHarness{processor: processor, pool: pool, embedder: embedder}
}

func (h *searchHarness) ingest(t *testing.T, in ragit.DocumentInput) *ragit.Document {
	t.Helper()
	doc, err := h.processor.Ingest(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, "ready", doc.Status)
	return doc
}

func TestVectorSearch_RanksByCosineSimilarity(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres tuning", search.Options{TopK: 5})
	require.NoError(t, err)
	require.NotEmpty(t, results)

	require.Equal(t, pgDoc.ID, results[0].DocumentID, "the postgres document must rank first")
	require.Equal(t, "db.md", results[0].Filename)
	require.InDelta(t, 1.0, results[0].Score, 1e-6, "an exact keyword-space match scores 1.0")
	require.Contains(t, results[0].Content, "Postgres")

	// The unrelated document is still returned (no cutoff was set) but scores
	// at the bottom — which is exactly why MinScore exists.
	require.Len(t, results, 2)
	require.Less(t, results[1].Score, 0.5)
}

func TestVectorSearch_MinScoreDropsWeakMatches(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres tuning", search.Options{
		TopK: 5, MinScore: 0.5,
	})
	require.NoError(t, err)
	require.Len(t, results, 1, "the orthogonal document falls below the cutoff")
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
}

func TestVectorSearch_IgnoresChunksFromAnotherEmbeddingSpace(t *testing.T) {
	h := newSearchHarness(t, "provider-a")
	tenantID := uuid.New()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	// Same wire format, same dimension, different provider label — so a
	// different fingerprint, and therefore a different embedding space.
	embedServer := newKeywordEmbedServer(t)
	defer embedServer.Close()
	otherEmbedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Provider: "provider-b", Dimension: searchEmbedDim,
	})
	require.NoError(t, err)
	require.NotEqual(t, embed.Fingerprint(h.embedder), embed.Fingerprint(otherEmbedder))

	searcher := search.New(h.pool, otherEmbedder)

	results, err := searcher.Vector(context.Background(), tenantID, "postgres tuning", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, results,
		"vectors from another provider are not comparable, so they must be excluded rather than ranked")

	// And the misalignment is detectable rather than merely silent.
	misaligned, err := searcher.CountMisalignedChunks(context.Background(), tenantID)
	require.NoError(t, err)
	require.Positive(t, misaligned)

	// The original embedder still sees its own corpus.
	aligned, err := search.New(h.pool, h.embedder).CountMisalignedChunks(context.Background(), tenantID)
	require.NoError(t, err)
	require.Zero(t, aligned)
}

func TestFullTextSearch_MatchesContentAndRanks(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.FullTextSearch(context.Background(), tenantID, "relational", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
	require.Positive(t, results[0].Score)

	// websearch_to_tsquery accepts what a user actually types, including
	// syntax that would make to_tsquery raise.
	results, err = h.processor.FullTextSearch(context.Background(), tenantID, `"relational data" -kubernetes`, search.Options{TopK: 5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
}

func TestFullTextSearch_UsesSimpleConfigNotEnglishStemming(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	// The 'simple' config does not stem, so the stored lexeme is "stores".
	// This test exists to catch the search_vector column and the query-time
	// config drifting apart: with an 'english' column and a 'simple' query,
	// the column would hold "store" and this exact-token search would find
	// nothing at all.
	exact, err := h.processor.FullTextSearch(context.Background(), tenantID, "stores", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Len(t, exact, 1, "the unstemmed token must match the unstemmed column")
}

func TestSearch_IsolatesTenants(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantA, tenantB := uuid.New(), uuid.New()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantA, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	vector, err := h.processor.VectorSearch(context.Background(), tenantB, "postgres tuning", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, vector, "tenant B must not retrieve tenant A's chunks")

	text, err := h.processor.FullTextSearch(context.Background(), tenantB, "relational", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, text)
}

func TestSearch_ExcludesSessionChunksUnlessAsked(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()
	sessionID := uuid.New()

	libraryDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "library.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	sessionDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, SessionID: &sessionID,
		Filename: "attachment.md", MimeType: "text/markdown",
		Data: []byte("Postgres attachment pasted into one conversation."),
	})

	// Default Options: an ephemeral attachment must not leak into an
	// ordinary library search just because the caller left a field unset.
	results, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{TopK: 10})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, libraryDoc.ID, results[0].DocumentID)

	// Asking for the session widens the result set to include it.
	results, err = h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{
		TopK: 10, SessionID: &sessionID,
	})
	require.NoError(t, err)
	require.Len(t, results, 2)

	found := map[uuid.UUID]bool{}
	for _, r := range results {
		found[r.DocumentID] = true
	}
	require.True(t, found[libraryDoc.ID])
	require.True(t, found[sessionDoc.ID], "the session's own attachment is visible to that session")

	// A different session sees only the library, not another session's file.
	otherSession := uuid.New()
	results, err = h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{
		TopK: 10, SessionID: &otherSession,
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, libraryDoc.ID, results[0].DocumentID)
}

func TestSearch_FiltersByScope(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()
	scopeA, scopeB := uuid.New(), uuid.New()

	docA := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeID: &scopeA, Filename: "a.md", MimeType: "text/markdown",
		Data: []byte("Postgres in project A."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeID: &scopeB, Filename: "b.md", MimeType: "text/markdown",
		Data: []byte("Postgres in project B."),
	})

	scoped, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{
		TopK: 10, ScopeID: &scopeA,
	})
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	require.Equal(t, docA.ID, scoped[0].DocumentID)

	// A nil ScopeID searches the whole tenant — scope is an organizational
	// filter, not the security boundary; tenant_id is.
	all, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{TopK: 10})
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestMoveDocumentScope_ResyncsChunks(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()
	oldScope, newScope := uuid.New(), uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeID: &oldScope, Filename: "a.md", MimeType: "text/markdown",
		Data: []byte("Postgres in the original project."),
	})

	require.NoError(t, h.processor.MoveDocumentScope(context.Background(), doc.ID, tenantID, &newScope, nil))

	// The chunks' denormalized copies moved with the document. This is the
	// case reprocessing would NOT have fixed: the resume check sees identical
	// content and skips the rewrite entirely.
	moved, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{
		TopK: 10, ScopeID: &newScope,
	})
	require.NoError(t, err)
	require.Len(t, moved, 1)
	require.Equal(t, doc.ID, moved[0].DocumentID)

	stale, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{
		TopK: 10, ScopeID: &oldScope,
	})
	require.NoError(t, err)
	require.Empty(t, stale, "chunks must not keep answering searches for the scope they left")
}

func TestVectorSearch_ReturnsCitationMetadata(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("# Handbook\n\n## Storage\n\nPostgres stores relational data durably.\n"),
	})

	results, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{TopK: 5})
	require.NoError(t, err)
	require.NotEmpty(t, results)

	top := results[0]
	require.Equal(t, doc.ID, top.DocumentID)
	require.Equal(t, "handbook.md", top.Filename)
	require.NotEqual(t, uuid.Nil, top.ChunkID)
	require.NotEmpty(t, top.HeadingPath, "citations need the heading trail the chunker recorded")
	require.Equal(t, []string{"Handbook", "Storage"}, top.HeadingPath)
}

// TestRLS_FailsClosedWithoutTenantScope pins down the property the whole
// WithTenant plumbing exists for. It is deliberately a database-level test,
// not an API-level one: the risk being guarded against is a future query
// added outside WithTenant, which no API test would catch.
func TestRLS_FailsClosedWithoutTenantScope(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	ctx := context.Background()

	// Inside a tenant transaction: visible.
	scoped := getChunks(t, h.pool, tenantID, doc.ID)
	require.NotEmpty(t, scoped)

	// Outside one — the tenant GUC never set — the policy evaluates to NULL
	// and the rows vanish rather than being exposed.
	var unscopedDocs, unscopedChunks int
	require.NoError(t, h.pool.QueryRow(ctx, "SELECT count(*) FROM ragit_documents").Scan(&unscopedDocs))
	require.NoError(t, h.pool.QueryRow(ctx, "SELECT count(*) FROM ragit_chunks").Scan(&unscopedChunks))
	require.Zero(t, unscopedDocs, "RLS must fail closed when no tenant scope is set")
	require.Zero(t, unscopedChunks)

	// A different tenant's transaction sees nothing either.
	require.NoError(t, db.WithTenant(ctx, h.pool, uuid.New(), func(q *db.Queries) error {
		rows, err := q.GetChunksByDocumentID(ctx, db.GetChunksByDocumentIDParams{
			DocumentID: doc.ID, TenantID: tenantID,
		})
		require.NoError(t, err)
		require.Empty(t, rows)
		return nil
	}))
}

// TestRLS_RejectsCrossTenantWrite proves the policy's WITH CHECK half: a row
// cannot be written for a tenant other than the one the transaction is
// scoped to, even though the INSERT itself supplies the tenant_id.
func TestRLS_RejectsCrossTenantWrite(t *testing.T) {
	pool := testutil.SetupTestPool(t)
	ctx := context.Background()

	scopedTenant, foreignTenant := uuid.New(), uuid.New()

	err := db.WithTenant(ctx, pool, scopedTenant, func(q *db.Queries) error {
		_, err := q.CreateDocument(ctx, db.CreateDocumentParams{
			TenantID: foreignTenant,
			Filename: "smuggled.md",
			MimeType: "text/markdown",
		})
		return err
	})
	require.Error(t, err)
	require.Contains(t, strings.ToLower(err.Error()), "row-level security")
}
