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
	"github.com/jryannel/ragit/store"
)

const searchEmbedDim = 1536

// searchKeywords give each keyword its own dimension in the mock embedding
// space, so a query containing one lands at cosine similarity 1.0 with
// documents containing it and 0.0 with the others. That makes ranking
// assertions exact rather than approximate.
var searchKeywords = []string{"postgres", "kubernetes", "tesseract"}

// newEchoExtractServer returns the uploaded bytes back as Markdown, so a test
// can ingest arbitrary text through one server.
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
				// Never a zero vector: cosine distance against it is undefined
				// and pgvector would return NaN scores.
				vec[searchEmbedDim-1] = 1
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
}

type harness struct {
	processor *ragit.Processor
	pool      *pgxpool.Pool
	embedder  embed.Embedder
	store     *store.MemoryStore
}

func newHarness(t *testing.T, provider string) *harness {
	t.Helper()
	pool := newHarnessPool(t)

	extractServer := newEchoExtractServer(t)
	t.Cleanup(extractServer.Close)
	embedServer := newKeywordEmbedServer(t)
	t.Cleanup(embedServer.Close)

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Provider: provider, Dimension: searchEmbedDim,
	})
	require.NoError(t, err)

	mem := store.NewMemoryStore()
	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 1000, Overlap: 0}),
		embedder, mem,
	)
	return &harness{processor: processor, pool: pool, embedder: embedder, store: mem}
}

func (h *harness) ingest(t *testing.T, in ragit.DocumentInput) *ragit.Document {
	t.Helper()
	doc, err := h.processor.Ingest(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, doc.Status)
	return doc
}

func TestVectorSearch_RanksByCosineSimilarity(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.VectorSearch(context.Background(),
		ragit.Tenant(tenantID), "postgres tuning", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, results, 2)

	require.Equal(t, pgDoc.ID, results[0].DocumentID, "the postgres document must rank first")
	require.Equal(t, "db.md", results[0].Filename, "the join must carry the filename for citations")
	require.InDelta(t, 1.0, results[0].Score, 1e-6, "an exact keyword-space match scores 1.0")
	require.Contains(t, results[0].Content, "Postgres")
	require.Less(t, results[1].Score, 0.5)
}

func TestVectorSearch_MinScoreDropsWeakMatches(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.VectorSearch(context.Background(),
		ragit.Tenant(tenantID), "postgres tuning", ragit.SearchOptions{TopK: 5, MinScore: 0.5})
	require.NoError(t, err)
	require.Len(t, results, 1, "the orthogonal document falls below the cutoff")
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
}

func TestVectorSearch_IgnoresChunksFromAnotherEmbeddingSpace(t *testing.T) {
	h := newHarness(t, "provider-a")
	tenantID := uuid.NewString()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	// Same wire format, same dimension, different provider label — so a
	// different fingerprint, and therefore a different embedding space.
	embedServer := newKeywordEmbedServer(t)
	defer embedServer.Close()
	other, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Provider: "provider-b", Dimension: searchEmbedDim,
	})
	require.NoError(t, err)
	require.NotEqual(t, embed.Fingerprint(h.embedder), embed.Fingerprint(other))

	otherProcessor := ragit.New(h.pool,
		extract.NewXbergExtractor("http://127.0.0.1:1", 0),
		chunk.New(chunk.DefaultConfig()), other, h.store)

	results, err := otherProcessor.VectorSearch(context.Background(),
		ragit.Tenant(tenantID), "postgres tuning", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, results,
		"vectors from another provider are not comparable, so they must be excluded rather than ranked")

	misaligned, err := otherProcessor.CountMisalignedChunks(context.Background(), ragit.Tenant(tenantID))
	require.NoError(t, err)
	require.Positive(t, misaligned, "the misalignment must be detectable rather than merely silent")

	aligned, err := h.processor.CountMisalignedChunks(context.Background(), ragit.Tenant(tenantID))
	require.NoError(t, err)
	require.Zero(t, aligned)
}

func TestFullTextSearch_MatchesContentAndRanks(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()

	pgDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "ops.md", MimeType: "text/markdown",
		Data: []byte("Kubernetes schedules containers across nodes."),
	})

	results, err := h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantID), "relational", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
	require.Positive(t, results[0].Score)

	// websearch_to_tsquery accepts what a user actually types, including
	// syntax that would make to_tsquery raise.
	results, err = h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantID), `"relational data" -kubernetes`, ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, pgDoc.ID, results[0].DocumentID)
}

func TestFullTextSearch_UsesSimpleConfigNotEnglishStemming(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	// 'simple' does not stem, so the stored lexeme is "stores". This test
	// exists to catch the search_vector column and the query-time config
	// drifting apart: with an 'english' column and a 'simple' query, the
	// column would hold "store" and this exact-token search would find nothing.
	exact, err := h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantID), "stores", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Len(t, exact, 1, "the unstemmed token must match the unstemmed column")
}

func TestSearch_IsolatesTenants(t *testing.T) {
	h := newHarness(t, "acme")
	tenantA, tenantB := uuid.NewString(), uuid.NewString()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantA, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	vector, err := h.processor.VectorSearch(context.Background(),
		ragit.Tenant(tenantB), "postgres tuning", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, vector, "tenant B must not retrieve tenant A's chunks")

	text, err := h.processor.FullTextSearch(context.Background(),
		ragit.Tenant(tenantB), "relational", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, text)
}

// The property the Scope type exists for: a call that forgot its confinement
// fails rather than returning everything.
func TestSearch_ZeroScopeIsRefused(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	ctx := context.Background()
	var forgotten ragit.Scope

	_, err := h.processor.VectorSearch(ctx, forgotten, "postgres", ragit.SearchOptions{})
	require.ErrorIs(t, err, ragit.ErrUnscoped)

	_, err = h.processor.FullTextSearch(ctx, forgotten, "postgres", ragit.SearchOptions{})
	require.ErrorIs(t, err, ragit.ErrUnscoped)

	_, err = h.processor.ListDocuments(ctx, forgotten, ragit.ListFilter{})
	require.ErrorIs(t, err, ragit.ErrUnscoped, "a catalog read is as much a boundary as a retrieval")

	_, err = h.processor.GetDocument(ctx, forgotten, uuid.NewString())
	require.ErrorIs(t, err, ragit.ErrUnscoped)
}

func TestSearch_ScopeDimensionsAreRestrictiveByDefault(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()
	companyA, companyB := uuid.NewString(), uuid.NewString()
	coach := uuid.NewString()

	unscoped := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "shared.md", MimeType: "text/markdown",
		Data: []byte("Postgres shared across the tenant."),
	})
	inA := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &companyA, Filename: "a.md", MimeType: "text/markdown",
		Data: []byte("Postgres for company A."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &companyB, Filename: "b.md", MimeType: "text/markdown",
		Data: []byte("Postgres for company B."),
	})
	pair := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &companyA, ScopeB: &coach,
		Filename: "pair.md", MimeType: "text/markdown",
		Data: []byte("Postgres for this coach at company A."),
	})

	ctx := context.Background()
	search := func(s ragit.Scope) []string {
		results, err := h.processor.VectorSearch(ctx, s, "postgres", ragit.SearchOptions{TopK: 20})
		require.NoError(t, err)
		ids := make([]string, len(results))
		for i, r := range results {
			ids[i] = r.DocumentID
		}
		return ids
	}

	// A dimension nobody mentioned matches only rows that have no value there.
	require.ElementsMatch(t, []string{unscoped.ID}, search(ragit.Tenant(tenantID)),
		"an unmentioned scope dimension must not return every scope's rows")

	// Naming a scope selects it — and still not the pair, whose scope B is set.
	require.ElementsMatch(t, []string{inA.ID}, search(ragit.Tenant(tenantID).A(companyA)))

	// The pair needs both halves named, which is the case a single scope
	// column could not express.
	require.ElementsMatch(t, []string{pair.ID}, search(ragit.Tenant(tenantID).A(companyA).B(coach)))

	// Unbounded access is a separate predicate, not a magic scope value.
	require.Len(t, search(ragit.Tenant(tenantID).AnyA().AnyB()), 4)

	// An empty permitted set matches nothing rather than everything — the
	// property that makes a computed-then-empty allow-list safe.
	require.Empty(t, search(ragit.Tenant(tenantID).A()))
}

func TestSearch_ExcludesSessionChunksUnlessAsked(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()
	sessionID := uuid.NewString()

	libraryDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "library.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	sessionDoc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, SessionID: &sessionID,
		Filename: "attachment.md", MimeType: "text/markdown",
		Data: []byte("Postgres attachment pasted into one conversation."),
	})

	ctx := context.Background()

	results, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, libraryDoc.ID, results[0].DocumentID,
		"an ephemeral attachment must not leak into an ordinary library search")

	results, err = h.processor.VectorSearch(ctx, ragit.Tenant(tenantID).Session(sessionID), "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Len(t, results, 2)

	found := map[string]bool{}
	for _, r := range results {
		found[r.DocumentID] = true
	}
	require.True(t, found[libraryDoc.ID])
	require.True(t, found[sessionDoc.ID])

	// A different session sees the library, not another session's file.
	results, err = h.processor.VectorSearch(ctx, ragit.Tenant(tenantID).Session(uuid.NewString()), "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, libraryDoc.ID, results[0].DocumentID)
}

func TestMoveDocumentScope_ResyncsChunks(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()
	oldScope, newScope := uuid.NewString(), uuid.NewString()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &oldScope, Filename: "a.md", MimeType: "text/markdown",
		Data: []byte("Postgres in the original project."),
	})

	ctx := context.Background()
	require.NoError(t, h.processor.MoveDocumentScope(ctx, tenantID, doc.ID, &newScope, nil, nil))

	// The chunks' denormalized copies moved with the document. This is the
	// case reprocessing would NOT have fixed: the resume check sees identical
	// content and skips the rewrite entirely.
	moved, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID).A(newScope), "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Len(t, moved, 1)
	require.Equal(t, doc.ID, moved[0].DocumentID)

	stale, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID).A(oldScope), "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Empty(t, stale, "chunks must not keep answering searches for the scope they left")
}

func TestVectorSearch_ReturnsCitationMetadata(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.NewString()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("# Handbook\n\n## Storage\n\nPostgres stores relational data durably.\n"),
	})

	results, err := h.processor.VectorSearch(context.Background(),
		ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.NotEmpty(t, results)

	top := results[0]
	require.Equal(t, doc.ID, top.DocumentID)
	require.Equal(t, "handbook.md", top.Filename)
	require.NotEmpty(t, top.ChunkID)
	require.Equal(t, []string{"Handbook", "Storage"}, top.HeadingPath,
		"citations need the heading trail the chunker recorded")
}
