package ragit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/embed"
	"github.com/mind-vm/ragit/internal/testutil"
	"github.com/mind-vm/ragit/store"
)

// preparedSpace stands in for a service that extracts, chunks and embeds in
// one call — the dimension matches ragit's shipped migrations so the fixture
// needs no regenerated schema.
var preparedSpace = embed.Space{Provider: "xberg", Model: "bge-base-en-v1.5", Dimension: 1536}

// preparedProcessor is the point of the whole entry point: no extractor, no
// chunker, no embedder. Nothing in this path runs the front half of the
// pipeline, so nothing here should have to supply one.
func preparedProcessor(t *testing.T) (*ragit.Processor, *pgxpool.Pool) {
	t.Helper()
	pool := testutil.SetupTestPool(t)
	return ragit.New(pool, nil, nil, nil, store.NewMemoryStore()), pool
}

func preparedChunks(contents ...string) []ragit.PreparedChunk {
	chunks := make([]ragit.PreparedChunk, len(contents))
	for i, c := range contents {
		vec := make(embed.Vector, preparedSpace.Dimension)
		vec[i%preparedSpace.Dimension] = 1
		chunks[i] = ragit.PreparedChunk{
			Content:     c,
			Embedding:   vec,
			HeadingPath: []string{"Handbook", "Accounts"},
			Metadata:    json.RawMessage(`{"page_spans":[[1,2]]}`),
		}
	}
	return chunks
}

func preparedDocument(contents ...string) ragit.PreparedDocument {
	return ragit.PreparedDocument{
		Text:     "the whole extracted document",
		Metadata: json.RawMessage(`{"extractor":"xberg"}`),
		Space:    preparedSpace,
		Chunks:   preparedChunks(contents...),
	}
}

func TestIngestPrepared_IndexesWithoutTheFrontHalfOfThePipeline(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID, scopeA := uuid.New(), uuid.New()
	sessionID := uuid.New()
	expires := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &scopeA, SessionID: &sessionID,
		Attributes: ragit.Attributes{"team": "warehouse"},
		ExpiresAt:  &expires,
		Filename:   "handbook.md", MimeType: "text/markdown",
		Data: []byte("irrelevant: this path never re-reads the bytes"),
	})
	require.NoError(t, err)

	prepared := preparedDocument("resetting a password", "requesting a new badge")
	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, prepared))

	scope := ragit.Tenant(tenantID).A(scopeA).Session(sessionID)
	doc, err := processor.GetDocument(ctx, scope, documentID)
	require.NoError(t, err)

	// The terminal state, which a hand-written writer sets field by field with
	// nothing checking the set is complete.
	require.Equal(t, ragit.StatusReady, doc.Status)
	require.NotNil(t, doc.ChunkCount)
	require.Equal(t, int32(2), *doc.ChunkCount)
	require.NotNil(t, doc.TextContent)
	require.Equal(t, prepared.Text, *doc.TextContent)
	require.NotNil(t, doc.EmbeddingModel)
	require.Equal(t, preparedSpace.Model, *doc.EmbeddingModel)
	require.NotNil(t, doc.ProcessedAt)
	require.JSONEq(t, `{"extractor":"xberg"}`, string(doc.Metadata))

	// The denormalized columns, each of which is silent when forgotten.
	chunks, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Len(t, chunks, 2)
	for i, c := range chunks {
		require.Equal(t, int32(i), c.ChunkIndex)
		require.Equal(t, tenantID, c.TenantID)
		require.Equal(t, &scopeA, c.ScopeAID)
		require.Equal(t, &sessionID, c.SessionID)
		require.NotNil(t, c.ExpiresAt)
		require.WithinDuration(t, expires, *c.ExpiresAt, time.Second)
		require.JSONEq(t, `{"team":"warehouse"}`, string(c.Attributes))
		require.JSONEq(t, `{"page_spans":[[1,2]]}`, string(c.Metadata))
		require.Equal(t, []string{"Handbook", "Accounts"}, c.HeadingPath)
		require.NotNil(t, c.EmbeddingFingerprint)
		require.Equal(t, "xberg|bge-base-en-v1.5|1536", *c.EmbeddingFingerprint)
	}
}

func TestIngestPrepared_ResumesAnUnchangedCorpus(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	prepared := preparedDocument("first", "second", "third")
	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, prepared))

	scope := ragit.Tenant(tenantID)
	before, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Len(t, before, 3)

	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, prepared))

	after, err := processor.ListChunks(ctx, scope, documentID)
	require.NoError(t, err)
	require.Len(t, after, 3, "a second pass must not duplicate chunks")
	for i := range after {
		require.Equal(t, before[i].ID, after[i].ID,
			"chunk %d was rewritten; an unchanged corpus should be left alone", i)
	}
}

func TestIngestPrepared_ReindexesWhenTheSpaceChanges(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	prepared := preparedDocument("first", "second")
	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, prepared))
	before, err := processor.ListChunks(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)

	// Same text, a different embedding model: the stored vectors are not
	// comparable with the new ones and must not survive alongside them.
	moved := prepared
	moved.Space = embed.Space{Provider: "xberg", Model: "e5-large-v2", Dimension: 1536}
	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, moved))

	after, err := processor.ListChunks(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Len(t, after, 2)
	for i, c := range after {
		require.NotEqual(t, before[i].ID, c.ID, "chunk %d should have been rewritten", i)
		require.Equal(t, "xberg|e5-large-v2|1536", *c.EmbeddingFingerprint)
	}

	doc, err := processor.GetDocument(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Equal(t, "e5-large-v2", *doc.EmbeddingModel)
}

func TestIngestPrepared_PublishesTheTerminalEvent(t *testing.T) {
	pool := testutil.SetupTestPool(t)
	ctx := context.Background()

	var events []ragit.Event
	processor := ragit.New(pool, nil, nil, nil, store.NewMemoryStore()).
		WithEventSink(ragit.EventSinkFunc(func(_ context.Context, e ragit.Event) {
			events = append(events, e)
		}))

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID, preparedDocument("only chunk")))

	require.Len(t, events, 1, "a host application's catalog learns about this document the same way")
	require.True(t, events[0].Succeeded())
	require.Equal(t, documentID, events[0].DocumentID)
	require.Equal(t, tenantID, events[0].TenantID)
	require.Equal(t, "handbook.md", events[0].Filename)
	require.Equal(t, 1, events[0].ChunkCount)
}

func TestIngestPrepared_MarksTheDocumentSkippedAboveTheChunkCap(t *testing.T) {
	pool := testutil.SetupTestPool(t)
	ctx := context.Background()

	var events []ragit.Event
	processor := ragit.New(pool, nil, nil, nil, store.NewMemoryStore()).
		WithMaxChunksPerDocument(2).
		WithEventSink(ragit.EventSinkFunc(func(_ context.Context, e ragit.Event) {
			events = append(events, e)
		}))

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "huge.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	require.NoError(t, processor.IngestPrepared(ctx, documentID, tenantID,
		preparedDocument("one", "two", "three")))

	doc, err := processor.GetDocument(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusSkippedTooLarge, doc.Status)
	require.NotNil(t, doc.Error)
	require.Contains(t, *doc.Error, "3 chunks")

	chunks, err := processor.ListChunks(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Empty(t, chunks)

	require.Len(t, events, 1)
	require.False(t, events[0].Succeeded())
	require.Equal(t, ragit.StatusSkippedTooLarge, events[0].Status)
}

func TestIngestPrepared_RejectsAMisdeclaredSpace(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	// A 1536-wide vector under a Space claiming 768. The column would take it
	// and every later query would filter on a fingerprint the corpus does not
	// actually live in — the failure that returns nothing and says nothing.
	wrong := preparedDocument("first")
	wrong.Space.Dimension = 768

	err = processor.IngestPrepared(ctx, documentID, tenantID, wrong)
	require.ErrorContains(t, err, "1536-dimension vector")
	require.ErrorContains(t, err, "Space declares 768")

	// A rejected call is the caller's bug, not the document's: the row is
	// untouched and still ingestable.
	doc, err := processor.GetDocument(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusPending, doc.Status)
}

func TestIngestPrepared_RejectsAnUnnamedSpace(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	unnamed := preparedDocument("first")
	unnamed.Space.Provider = ""

	require.ErrorContains(t, processor.IngestPrepared(ctx, documentID, tenantID, unnamed),
		"Space.Provider is required")
}

func TestProcessDocument_ReportsMissingDependencies(t *testing.T) {
	processor, _ := preparedProcessor(t)
	ctx := context.Background()

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte("bytes"),
	})
	require.NoError(t, err)

	// The prepared path builds a Processor without the front half of the
	// pipeline. Asking that Processor to run it must say so, not panic.
	require.ErrorContains(t, processor.ProcessDocument(ctx, documentID, tenantID),
		"needs an extractor, a chunker and an embedder")

	_, err = processor.VectorSearch(ctx, ragit.Tenant(tenantID), "a query", ragit.SearchOptions{})
	require.ErrorContains(t, err, "without an embedder")
}
