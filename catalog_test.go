package ragit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/extract"
	"github.com/jryannel/ragit/store"
)

func TestCatalog_ListAndGetDocuments(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	ctx := context.Background()

	first := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "one.md", MimeType: "text/markdown",
		Data: []byte("Postgres one."),
	})
	second := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "two.md", MimeType: "text/markdown",
		Data: []byte("Postgres two."),
	})

	scope := ragit.Tenant(tenantID)

	docs, err := h.processor.ListDocuments(ctx, scope, ragit.ListFilter{})
	require.NoError(t, err)
	require.Len(t, docs, 2)

	count, err := h.processor.CountDocuments(ctx, scope, ragit.ListFilter{})
	require.NoError(t, err)
	require.EqualValues(t, 2, count)

	// The three questions a catalog exists to answer.
	got, err := h.processor.GetDocument(ctx, scope, first.ID)
	require.NoError(t, err)
	require.Equal(t, "one.md", got.Filename)        // what is indexed
	require.Equal(t, ragit.StatusReady, got.Status) // is it still processing
	require.Nil(t, got.Error)                       // why did it fail
	require.NotNil(t, got.TextContent)
	require.NotNil(t, got.ChunkCount)

	byStatus, err := h.processor.ListDocuments(ctx, scope,
		ragit.ListFilter{Status: []string{ragit.StatusReady}})
	require.NoError(t, err)
	require.Len(t, byStatus, 2)

	none, err := h.processor.ListDocuments(ctx, scope,
		ragit.ListFilter{Status: []string{ragit.StatusError}})
	require.NoError(t, err)
	require.Empty(t, none)

	paged, err := h.processor.ListDocuments(ctx, scope, ragit.ListFilter{Limit: 1})
	require.NoError(t, err)
	require.Len(t, paged, 1)

	_ = second
}

// A catalog read is a confinement boundary too: a document that exists but is
// out of scope is indistinguishable from one that does not exist, because
// saying "it exists, but not for you" is itself a disclosure.
func TestCatalog_OutOfScopeIsIndistinguishableFromMissing(t *testing.T) {
	h := newHarness(t, "acme")
	tenantA, tenantB := uuid.New(), uuid.New()
	ctx := context.Background()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantA, Filename: "private.md", MimeType: "text/markdown",
		Data: []byte("Postgres for tenant A only."),
	})

	_, realMiss := h.processor.GetDocument(ctx, ragit.Tenant(tenantB), uuid.New())
	_, crossTenant := h.processor.GetDocument(ctx, ragit.Tenant(tenantB), doc.ID)

	require.ErrorIs(t, realMiss, ragit.ErrNotFound)
	require.ErrorIs(t, crossTenant, ragit.ErrNotFound)

	docs, err := h.processor.ListDocuments(ctx, ragit.Tenant(tenantB), ragit.ListFilter{})
	require.NoError(t, err)
	require.Empty(t, docs)
}

// "Why did this fail?" is the third question the catalog exists to answer, and
// the one that is useless unless the reason survives onto the row.
func TestCatalog_SurfacesFailureReason(t *testing.T) {
	pool := newHarnessPool(t)
	ctx := context.Background()

	extractServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error_type":"ParsingError","message":"password-protected document"}`))
	}))
	defer extractServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "unused"})
	require.NoError(t, err)
	processor := ragit.New(pool, extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.DefaultConfig()), embedder, store.NewMemoryStore())

	tenantID := uuid.New()
	_, err = processor.Ingest(ctx, ragit.DocumentInput{
		TenantID: tenantID, Filename: "locked.pdf", MimeType: "application/pdf",
		Data: []byte("%PDF-1.4 encrypted"),
	})
	require.Error(t, err)

	docs, err := processor.ListDocuments(ctx, ragit.Tenant(tenantID),
		ragit.ListFilter{Status: []string{ragit.StatusError}})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, "locked.pdf", docs[0].Filename)
	require.NotNil(t, docs[0].Error)
	require.Contains(t, *docs[0].Error, "password-protected document",
		"the reason must reach the person who uploaded the file")
}
