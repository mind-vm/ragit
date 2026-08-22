package ragit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	"github.com/jryannel/ragit/store"
)

// markdownFixture has enough headings to produce several chunks with a
// small chunk size, exercising SplitMarkdown's heading-path tracking.
const markdownFixture = `# Intro

This is the introduction section, with enough text to fill most of a small chunk on its own so the splitter has real content to work with here.

## Background

Some background detail that goes on for a little while, giving the recursive splitter something to chew on across a paragraph boundary or two.

## Details

More detail content in this final section, again long enough to matter for chunk boundaries in the test.
`

func TestIngest_EndToEnd(t *testing.T) {
	pool := testutil.SetupTestPool(t)

	extractServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/extract", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{
					"content":   markdownFixture,
					"mime_type": "text/markdown",
					"metadata":  map[string]any{"format": map[string]any{"page_count": 1}},
				},
			},
			"errors": []any{},
		})
	}))
	defer extractServer.Close()

	const embedDim = 1536
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, embedDim)
			for j := range vec {
				vec[j] = float32(i+1) / float32(j+1) // deterministic, non-zero
			}
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer embedServer.Close()

	extractor := extract.NewXbergExtractor(extractServer.URL, 0)
	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey:    "test-key",
		BaseURL:   embedServer.URL,
		Dimension: embedDim,
	})
	require.NoError(t, err)
	chunker := chunk.New(chunk.Config{Size: 200, Overlap: 20})
	mem := store.NewMemoryStore()

	processor := ragit.New(pool, extractor, chunker, embedder, mem)

	tenantID := uuid.New()
	doc, err := processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown", Data: []byte(markdownFixture),
	})
	require.NoError(t, err)
	require.Equal(t, "ready", doc.Status)
	require.NotZero(t, doc.ChunkCount)
	require.Equal(t, embed.DefaultModel, doc.EmbeddingModel)

	row := getDoc(t, pool, tenantID, doc.ID)
	require.Equal(t, "ready", row.Status)
	require.NotNil(t, row.ChunkCount)
	require.Equal(t, doc.ChunkCount, int(*row.ChunkCount))

	chunks := getChunks(t, pool, tenantID, doc.ID)
	require.Len(t, chunks, doc.ChunkCount)

	wantFingerprint := embed.Fingerprint(embedder)
	for _, c := range chunks {
		require.NotNil(t, c.Embedding)
		require.Len(t, c.Embedding.Slice(), embedDim)
		require.NotNil(t, c.EmbeddingFingerprint)
		require.Equal(t, wantFingerprint, *c.EmbeddingFingerprint)
	}

	// Sanity: heading-aware chunking actually produced a heading path, not
	// just a flat character split.
	var sawHeading bool
	for _, c := range chunks {
		if len(c.HeadingPath) > 0 {
			sawHeading = true
			break
		}
	}
	require.True(t, sawHeading, "expected at least one chunk to carry a heading path")
}

func TestIngest_ExtractorRejectsDocument_MarksDocumentError(t *testing.T) {
	pool := testutil.SetupTestPool(t)

	extractServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error_type": "ParsingError",
			"message":    "corrupt file",
		})
	}))
	defer extractServer.Close()

	extractor := extract.NewXbergExtractor(extractServer.URL, 0)
	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "unused"})
	require.NoError(t, err)
	processor := ragit.New(pool, extractor, chunk.New(chunk.DefaultConfig()), embedder, store.NewMemoryStore())

	tenantID := uuid.New()
	doc, err := processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "broken.pdf", MimeType: "application/pdf", Data: []byte("not a real pdf"),
	})
	require.Error(t, err) // the attempt failed — Ingest must not swallow it
	require.NotNil(t, doc, "the document row is still returned, reflecting its persisted error state")
	require.Equal(t, "error", doc.Status)
	require.True(t, strings.Contains(doc.Error, "corrupt file"), "error was: %s", doc.Error)
}

// longFixture has no headings, so it chunks via SplitText's recursive
// splitter. With a small chunk size it reliably produces enough chunks to
// span multiple embedBatchSize(=10) batches, which the resume test needs.
var longFixture = strings.Repeat("Lorem ipsum dolor sit amet, consectetur adipiscing elit. ", 60)

func newExtractServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"content": text, "mime_type": "text/markdown"}},
			"errors":  []any{},
		})
	}))
}

// newEmbedServer returns a mock embeddings endpoint plus a running count of
// how many texts it has successfully embedded (i.e. excluding any request
// that got a simulated failure) — the tool the resume test uses to prove a
// chunk already persisted is never sent for embedding again. Counting
// totals rather than deduplicating by text content on purpose: fixture text
// can legitimately produce byte-identical chunks, which would make a
// seen-text-content check indistinguishable from a real double-embed.
func newEmbedServer(t *testing.T, dim int, failOnCall int) (server *httptest.Server, totalEmbedded func() int) {
	t.Helper()
	var mu sync.Mutex
	total := 0
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))

		mu.Lock()
		calls++
		thisCall := calls
		mu.Unlock()

		if failOnCall > 0 && thisCall == failOnCall {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("simulated failure"))
			return
		}

		mu.Lock()
		total += len(req.Input)
		mu.Unlock()

		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			vec := make([]float32, dim)
			vec[0] = 1 // non-zero, deterministic
			data[i] = map[string]any{"index": i, "embedding": vec}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return total
	}
}

func TestProcessDocument_ResumesAfterPartialEmbedFailure(t *testing.T) {
	pool := testutil.SetupTestPool(t)

	extractServer := newExtractServer(t, longFixture)
	defer extractServer.Close()

	const embedDim = 1536 // matches the fixed vector(1536) column in migrations/00001
	// Fail on the 2nd embed call: batch 1 (chunks 0-9) succeeds, batch 2 fails.
	embedServer, totalEmbedded := newEmbedServer(t, embedDim, 2)
	defer embedServer.Close()

	extractor := extract.NewXbergExtractor(extractServer.URL, 0)
	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: embedServer.URL, Dimension: embedDim})
	require.NoError(t, err)
	chunker := chunk.New(chunk.Config{Size: 100, Overlap: 10})
	processor := ragit.New(pool, extractor, chunker, embedder, store.NewMemoryStore())

	wantChunks := chunker.SplitText(longFixture)
	require.Greaterf(t, len(wantChunks), 10, "fixture must span at least two embedBatchSize(10) batches")

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "big.md", MimeType: "text/markdown", Data: []byte(longFixture),
	})
	require.NoError(t, err)

	// First attempt: fails partway through embedding.
	err = processor.ProcessDocument(context.Background(), documentID, tenantID)
	require.Error(t, err)

	doc := getDoc(t, pool, tenantID, documentID)
	require.Equal(t, "error", doc.Status)

	partial := getChunks(t, pool, tenantID, documentID)
	require.Len(t, partial, 10, "the first, successful batch should already be persisted")

	// Second attempt: the mock is healthy from here on.
	err = processor.ProcessDocument(context.Background(), documentID, tenantID)
	require.NoError(t, err)

	doc = getDoc(t, pool, tenantID, documentID)
	require.Equal(t, "ready", doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.Equal(t, len(wantChunks), int(*doc.ChunkCount))

	final := getChunks(t, pool, tenantID, documentID)
	require.Len(t, final, len(wantChunks))

	// The core claim of this phase: across both attempts, exactly one text
	// was embedded per chunk — the first batch was never billed again on
	// the retry, only the remaining, not-yet-embedded chunks were.
	require.Equal(t, len(wantChunks), totalEmbedded())
}

func TestProcessDocument_MaxChunksPerDocument_SkipsTooLarge(t *testing.T) {
	pool := testutil.SetupTestPool(t)

	extractServer := newExtractServer(t, longFixture)
	defer extractServer.Close()

	embedCalled := false
	embedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		embedCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer embedServer.Close()

	extractor := extract.NewXbergExtractor(extractServer.URL, 0)
	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: embedServer.URL})
	require.NoError(t, err)
	chunker := chunk.New(chunk.Config{Size: 100, Overlap: 10})
	processor := ragit.New(pool, extractor, chunker, embedder, store.NewMemoryStore()).WithMaxChunksPerDocument(3)

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "big.md", MimeType: "text/markdown", Data: []byte(longFixture),
	})
	require.NoError(t, err)

	err = processor.ProcessDocument(context.Background(), documentID, tenantID)
	require.NoError(t, err, "the guardrail is a deliberate quarantine, not a Go error")
	require.False(t, embedCalled, "embedding must never be called once the chunk cap is exceeded")

	doc := getDoc(t, pool, tenantID, documentID)
	require.Equal(t, "skipped_too_large", doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.Zero(t, *doc.ChunkCount)

	chunks := getChunks(t, pool, tenantID, documentID)
	require.Empty(t, chunks)
}

// getDoc and getChunks read through a tenant-scoped transaction. Reading with
// a plain db.New(pool) would return zero rows rather than failing loudly,
// because the RLS policies fail closed when no tenant GUC is set — which is
// the behaviour TestRLS_* below pins down deliberately.
func getDoc(t *testing.T, pool *pgxpool.Pool, tenantID, documentID uuid.UUID) db.Document {
	t.Helper()
	var doc db.Document
	require.NoError(t, db.WithTenant(context.Background(), pool, tenantID, func(q *db.Queries) error {
		var err error
		doc, err = q.GetDocumentByID(context.Background(), db.GetDocumentByIDParams{ID: documentID, TenantID: tenantID})
		return err
	}))
	return doc
}

func getChunks(t *testing.T, pool *pgxpool.Pool, tenantID, documentID uuid.UUID) []db.Chunk {
	t.Helper()
	var chunks []db.Chunk
	require.NoError(t, db.WithTenant(context.Background(), pool, tenantID, func(q *db.Queries) error {
		var err error
		chunks, err = q.GetChunksByDocumentID(context.Background(), db.GetChunksByDocumentIDParams{DocumentID: documentID, TenantID: tenantID})
		return err
	}))
	return chunks
}
