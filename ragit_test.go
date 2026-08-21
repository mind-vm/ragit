package ragit_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	doc, err := processor.Ingest(context.Background(), tenantID, "handbook.md", "text/markdown", []byte(markdownFixture))
	require.NoError(t, err)
	require.Equal(t, "ready", doc.Status)
	require.NotZero(t, doc.ChunkCount)
	require.Equal(t, embed.DefaultModel, doc.EmbeddingModel)

	queries := db.New(pool)
	row, err := queries.GetDocumentByID(context.Background(), db.GetDocumentByIDParams{ID: doc.ID, TenantID: tenantID})
	require.NoError(t, err)
	require.Equal(t, "ready", row.Status)
	require.NotNil(t, row.ChunkCount)
	require.Equal(t, doc.ChunkCount, int(*row.ChunkCount))

	chunks, err := queries.GetChunksByDocumentID(context.Background(), db.GetChunksByDocumentIDParams{DocumentID: doc.ID, TenantID: tenantID})
	require.NoError(t, err)
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
	doc, err := processor.Ingest(context.Background(), tenantID, "broken.pdf", "application/pdf", []byte("not a real pdf"))
	require.NoError(t, err) // Ingest reports failure on the document, not as a Go error
	require.Equal(t, "error", doc.Status)
	require.True(t, strings.Contains(doc.Error, "corrupt file"), "error was: %s", doc.Error)
}
