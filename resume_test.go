package ragit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/chunk"
	"github.com/mind-vm/ragit/embed"
	"github.com/mind-vm/ragit/extract"
	"github.com/mind-vm/ragit/internal/testutil"
	"github.com/mind-vm/ragit/store"
)

// resumeFixture is the corpus every test here ingests: small chunks, several
// of them, so a partial reuse is visible rather than all-or-nothing.
type resumeFixture struct {
	processor  *ragit.Processor
	pool       *pgxpool.Pool
	embedder   embed.Embedder
	tenantID   uuid.UUID
	scope      ragit.Scope
	documentID uuid.UUID
	contents   []string
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	pool := testutil.SetupTestPool(t)

	extractServer := newExtractServer(t, markdownFixture)
	t.Cleanup(extractServer.Close)
	const embedDim = 1536
	embedServer, _ := newEmbedServer(t, embedDim, 0)
	t.Cleanup(embedServer.Close)

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Dimension: embedDim,
	})
	require.NoError(t, err)

	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 200, Overlap: 20}),
		embedder, store.NewMemoryStore(),
	)

	tenantID := uuid.New()
	doc, err := processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte(markdownFixture),
	})
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, doc.Status)

	scope := ragit.Tenant(tenantID)
	stored, err := processor.ListChunks(context.Background(), scope, doc.ID)
	require.NoError(t, err)
	require.Greater(t, len(stored), 2, "fixture must chunk into more than two pieces")

	contents := make([]string, len(stored))
	for i, c := range stored {
		require.Equal(t, int32(i), c.ChunkIndex, "stored chunks must be indexed contiguously from zero")
		contents[i] = c.Content
	}

	return &resumeFixture{
		processor: processor, pool: pool, embedder: embedder,
		tenantID: tenantID, scope: scope, documentID: doc.ID, contents: contents,
	}
}

// resume runs the guard the way a bypassing caller would: on its own executor,
// inside its own tenant-scoped transaction.
func (f *resumeFixture) resume(t *testing.T, contents []string, fingerprint string) []bool {
	t.Helper()
	var reusable []bool
	require.NoError(t, ragit.WithTenant(context.Background(), f.pool, f.tenantID, func(db sqlb.Executor) error {
		var err error
		reusable, err = ragit.ResumeChunks(context.Background(), db, f.tenantID, f.documentID, contents, fingerprint)
		return err
	}))
	return reusable
}

func (f *resumeFixture) storedChunks(t *testing.T) []ragit.Chunk {
	t.Helper()
	stored, err := f.processor.ListChunks(context.Background(), f.scope, f.documentID)
	require.NoError(t, err)
	return stored
}

func TestResumeChunks_ReusesEveryMatchingChunk(t *testing.T) {
	f := newResumeFixture(t)

	reusable := f.resume(t, f.contents, embed.Fingerprint(f.embedder))

	require.Len(t, reusable, len(f.contents))
	for i, ok := range reusable {
		require.True(t, ok, "chunk %d is stored unchanged and should be reusable", i)
	}
	require.Len(t, f.storedChunks(t), len(f.contents), "nothing should have been cleared")
}

func TestResumeChunks_WipesOnFingerprintMismatch(t *testing.T) {
	f := newResumeFixture(t)

	// Same text, different embedding space: the corpus is unusable, and
	// keeping any of it would mix two spaces inside one document.
	reusable := f.resume(t, f.contents, "other|model|1536")

	require.Len(t, reusable, len(f.contents))
	for i, ok := range reusable {
		require.False(t, ok, "chunk %d was embedded by another embedder", i)
	}
	require.Empty(t, f.storedChunks(t), "a fingerprint mismatch clears the document")
}

func TestResumeChunks_WipesWhenContentChanged(t *testing.T) {
	f := newResumeFixture(t)

	// One chunk re-chunked differently. Every chunk goes, not just this one:
	// a later index may have shifted content the comparison cannot see.
	changed := append([]string(nil), f.contents...)
	changed[len(changed)-1] = "the document was edited"

	reusable := f.resume(t, changed, embed.Fingerprint(f.embedder))

	for i, ok := range reusable {
		require.False(t, ok, "chunk %d should not survive a re-chunk", i)
	}
	require.Empty(t, f.storedChunks(t), "changed content clears the document")
}

func TestResumeChunks_WipesWhenDocumentShrinks(t *testing.T) {
	f := newResumeFixture(t)

	// The fresh set is shorter than what is stored, so the trailing stored
	// chunks index past its end. They would otherwise stay behind, answering
	// searches for text the document no longer contains.
	shorter := f.contents[:len(f.contents)-1]

	reusable := f.resume(t, shorter, embed.Fingerprint(f.embedder))

	require.Len(t, reusable, len(shorter))
	for i, ok := range reusable {
		require.False(t, ok, "chunk %d should not survive a shrinking document", i)
	}
	require.Empty(t, f.storedChunks(t), "an orphaned trailing chunk clears the document")
}

// TestResumeChunks_ResumesAHandWrittenCorpus is the case the guard was exported
// for: a caller whose chunks and vectors were produced outside ragit — an
// extraction service that chunks and embeds in one call — writes them with
// sqlb, and on the next run pays only for what is missing.
func TestResumeChunks_ResumesAHandWrittenCorpus(t *testing.T) {
	pool := testutil.SetupTestPool(t)
	ctx := context.Background()

	extractServer := newExtractServer(t, markdownFixture)
	t.Cleanup(extractServer.Close)
	embedServer, embeddedTexts := newEmbedServer(t, 1536, 0)
	t.Cleanup(embedServer.Close)

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Dimension: 1536,
	})
	require.NoError(t, err)

	// A Processor for the document row only. This path never chunks and never
	// embeds a corpus, which is why the guard takes an executor and a
	// fingerprint rather than reading either off a Processor.
	processor := ragit.New(pool,
		extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.DefaultConfig()), embedder, store.NewMemoryStore())

	tenantID := uuid.New()
	documentID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "handbook.md", MimeType: "text/markdown",
		Data: []byte(markdownFixture),
	})
	require.NoError(t, err)

	const fingerprint = "xberg|bge-base-en-v1.5|1536"
	contents := []string{"first prepared chunk", "second prepared chunk", "third prepared chunk"}

	// First pass: nothing stored, so everything is the caller's to write.
	first := writePrepared(ctx, t, pool, tenantID, documentID, contents, fingerprint)
	require.Equal(t, []int{0, 1, 2}, first)

	// Second pass: same corpus, same space — nothing left to pay for.
	second := writePrepared(ctx, t, pool, tenantID, documentID, contents, fingerprint)
	require.Empty(t, second, "an unchanged corpus must cost nothing on a second pass")

	stored, err := processor.ListChunks(ctx, ragit.Tenant(tenantID), documentID)
	require.NoError(t, err)
	require.Len(t, stored, len(contents), "the second pass must not duplicate chunks")
	require.Equal(t, 0, embeddedTexts(), "this corpus never goes through an Embedder at all")
}

// writePrepared is the bypassing caller: guard and inserts in one transaction,
// returning the indices it had to write.
func writePrepared(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenantID, documentID uuid.UUID, contents []string, fingerprint string) []int {
	t.Helper()
	var written []int
	require.NoError(t, ragit.WithTenant(ctx, pool, tenantID, func(db sqlb.Executor) error {
		reusable, err := ragit.ResumeChunks(ctx, db, tenantID, documentID, contents, fingerprint)
		if err != nil {
			return err
		}

		rows := make([]*ragit.Chunk, 0, len(contents))
		for i, content := range contents {
			if reusable[i] {
				continue
			}
			written = append(written, i)
			vec := sqlb.Vector(prepareVector(i))
			fp := fingerprint
			rows = append(rows, &ragit.Chunk{
				DocumentID: documentID, TenantID: tenantID,
				ChunkIndex: int32(i), Content: content,
				Embedding: &vec, EmbeddingFingerprint: &fp,
				Metadata: json.RawMessage("{}"), Attributes: json.RawMessage("{}"),
			})
		}
		if len(rows) == 0 {
			return nil
		}
		_, err = sqlb.InsertRows(rows...).Omit("id", "created_at", "metadata", "attributes").Exec(ctx, db)
		return err
	}))
	return written
}

// prepareVector stands in for vectors an extraction service returned.
func prepareVector(i int) []float32 {
	vec := make([]float32, 1536)
	vec[i%1536] = 1
	return vec
}
