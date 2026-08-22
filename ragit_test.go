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
	"github.com/jryannel/ragit/internal/testutil"
	"github.com/jryannel/ragit/store"
)

// markdownFixture has enough headings to produce several chunks with a small
// chunk size, exercising SplitMarkdown's heading-path tracking.
const markdownFixture = `# Intro

This is the introduction section, with enough text to fill most of a small chunk on its own so the splitter has real content to work with here.

## Background

Some background detail that goes on for a little while, giving the recursive splitter something to chew on across a paragraph boundary or two.

## Details

More detail content in this final section, again long enough to matter for chunk boundaries in the test.
`

func TestIngest_EndToEnd(t *testing.T) {
	pool := testutil.SetupTestPool(t)

	extractServer := newExtractServer(t, markdownFixture)
	defer extractServer.Close()

	const embedDim = 1536
	embedServer, _ := newEmbedServer(t, embedDim, 0)
	defer embedServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "test-key", BaseURL: embedServer.URL, Dimension: embedDim,
	})
	require.NoError(t, err)

	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 200, Overlap: 20}),
		embedder,
		store.NewMemoryStore(),
	)

	tenantID := uuid.New()
	doc, err := processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte(markdownFixture),
	})
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.NotZero(t, *doc.ChunkCount)
	require.NotNil(t, doc.EmbeddingModel)
	require.Equal(t, embed.DefaultModel, *doc.EmbeddingModel)

	// The generated model carries the extracted text and metadata, so a
	// consumer never needs a bespoke accessor for them.
	require.NotNil(t, doc.TextContent)
	require.Contains(t, *doc.TextContent, "# Intro")

	scope := ragit.Tenant(tenantID)
	chunks, err := processor.ListChunks(context.Background(), scope, doc.ID)
	require.NoError(t, err)
	require.Len(t, chunks, int(*doc.ChunkCount))

	wantFingerprint := embed.Fingerprint(embedder)
	for _, c := range chunks {
		require.NotNil(t, c.Embedding)
		require.Len(t, *c.Embedding, embedDim)
		require.NotNil(t, c.EmbeddingFingerprint)
		require.Equal(t, wantFingerprint, *c.EmbeddingFingerprint)
	}

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
			"error_type": "ParsingError", "message": "corrupt file",
		})
	}))
	defer extractServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "unused"})
	require.NoError(t, err)
	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.DefaultConfig()), embedder, store.NewMemoryStore())

	tenantID := uuid.New()
	doc, err := processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "broken.pdf", MimeType: "application/pdf",
		Data: []byte("not a real pdf"),
	})
	require.Error(t, err, "the attempt failed — Ingest must not swallow it")
	require.NotNil(t, doc, "the row is still returned, reflecting its persisted error state")
	require.Equal(t, ragit.StatusError, doc.Status)
	require.NotNil(t, doc.Error)
	require.Contains(t, *doc.Error, "corrupt file")
}

// longFixture has no headings, so it chunks via the recursive splitter. With a
// small chunk size it reliably spans multiple embedBatchSize(=10) batches.
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
// texts successfully embedded (excluding any request that got a simulated
// failure) — the tool the resume test uses to prove a chunk already persisted
// is never sent for embedding again. Counting totals rather than deduplicating
// by content on purpose: fixture text can legitimately produce byte-identical
// chunks, which would make a seen-content check indistinguishable from a real
// double-embed.
func newEmbedServer(t *testing.T, dim int, failOnCall int) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	total, calls := 0, 0

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
			vec[0] = 1
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

	const embedDim = 1536
	// Fail on the 2nd embed call: batch 1 (chunks 0-9) succeeds, batch 2 fails.
	embedServer, totalEmbedded := newEmbedServer(t, embedDim, 2)
	defer embedServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Dimension: embedDim,
	})
	require.NoError(t, err)
	chunker := chunk.New(chunk.Config{Size: 100, Overlap: 10})
	processor := ragit.New(pool, extract.NewXbergExtractor(extractServer.URL, 0),
		chunker, embedder, store.NewMemoryStore())

	wantChunks := chunker.SplitText(longFixture)
	require.Greater(t, len(wantChunks), 10, "fixture must span at least two batches")

	ctx := context.Background()
	tenantID := uuid.New()
	scope := ragit.Tenant(tenantID)

	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "big.md", MimeType: "text/markdown",
		Data: []byte(longFixture),
	})
	require.NoError(t, err)

	// First attempt: fails partway through embedding.
	require.Error(t, processor.ProcessDocument(ctx, documentID, tenantID))

	doc, err := processor.GetDocument(ctx, scope, documentID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusError, doc.Status)

	partial, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Len(t, partial, 10, "the first, successful batch should already be persisted")

	// Second attempt: the mock is healthy from here on.
	require.NoError(t, processor.ProcessDocument(ctx, documentID, tenantID))

	doc, err = processor.GetDocument(ctx, scope, documentID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.Equal(t, len(wantChunks), int(*doc.ChunkCount))

	final, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Len(t, final, len(wantChunks))

	// The core claim: across both attempts, exactly one text was embedded per
	// chunk — the first batch was never billed again on the retry.
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

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "k", BaseURL: embedServer.URL})
	require.NoError(t, err)
	processor := ragit.New(pool, extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 100, Overlap: 10}), embedder, store.NewMemoryStore()).
		WithMaxChunksPerDocument(3)

	ctx := context.Background()
	tenantID := uuid.New()

	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "big.md", MimeType: "text/markdown",
		Data: []byte(longFixture),
	})
	require.NoError(t, err)

	require.NoError(t, processor.ProcessDocument(ctx, documentID, tenantID),
		"the guardrail is a deliberate quarantine, not a Go error")
	require.False(t, embedCalled, "embedding must never be called once the chunk cap is exceeded")

	scope := ragit.Tenant(tenantID)
	doc, err := processor.GetDocument(ctx, scope, documentID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusSkippedTooLarge, doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.Zero(t, *doc.ChunkCount)

	chunks, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Empty(t, chunks)
}

// newHarnessPool is shared by the other root test files.
func newHarnessPool(t *testing.T) *pgxpool.Pool { return testutil.SetupTestPool(t) }
