package ragit_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
)

func TestAttributes_NarrowVectorAndFullTextSearch(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	ctx := context.Background()

	intro := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "intro.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"course": "intro", "lang": "en"},
		Data:       []byte("Postgres for beginners."),
	})
	advanced := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "advanced.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"course": "advanced", "lang": "en"},
		Data:       []byte("Postgres query planning in depth."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "none.md", MimeType: "text/markdown",
		Data: []byte("Postgres with no attributes at all."),
	})

	scope := ragit.Tenant(tenantID)

	// Empty filter narrows nothing: attributes are not a boundary.
	all, err := h.processor.VectorSearch(ctx, scope, "postgres", ragit.SearchOptions{TopK: 10})
	require.NoError(t, err)
	require.Len(t, all, 3)

	one, err := h.processor.VectorSearch(ctx, scope, "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "intro"},
	})
	require.NoError(t, err)
	require.Len(t, one, 1)
	require.Equal(t, intro.ID, one[0].DocumentID)

	// Containment, not equality: naming a subset of a document's pairs matches.
	byLang, err := h.processor.VectorSearch(ctx, scope, "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"lang": "en"},
	})
	require.NoError(t, err)
	require.Len(t, byLang, 2, "the document with no attributes is excluded")

	// Multiple pairs are ANDed.
	both, err := h.processor.VectorSearch(ctx, scope, "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "advanced", "lang": "en"},
	})
	require.NoError(t, err)
	require.Len(t, both, 1)
	require.Equal(t, advanced.ID, both[0].DocumentID)

	// A pair nothing carries matches nothing.
	none, err := h.processor.VectorSearch(ctx, scope, "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "nonexistent"},
	})
	require.NoError(t, err)
	require.Empty(t, none)

	// Full-text search filters the same way.
	text, err := h.processor.FullTextSearch(ctx, scope, "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "intro"},
	})
	require.NoError(t, err)
	require.Len(t, text, 1)
	require.Equal(t, intro.ID, text[0].DocumentID)
}

func TestAttributes_NarrowCatalogListing(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	ctx := context.Background()

	wanted := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "a.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"kind": "recording"},
		Data:       []byte("Postgres one."),
	})
	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "b.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"kind": "handout"},
		Data:       []byte("Postgres two."),
	})

	docs, err := h.processor.ListDocuments(ctx, ragit.Tenant(tenantID), ragit.ListFilter{
		Attributes: ragit.Attributes{"kind": "recording"},
	})
	require.NoError(t, err)
	require.Len(t, docs, 1)
	require.Equal(t, wanted.ID, docs[0].ID)

	count, err := h.processor.CountDocuments(ctx, ragit.Tenant(tenantID), ragit.ListFilter{
		Attributes: ragit.Attributes{"kind": "recording"},
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, count)

	// Attributes round-trip off the stored row.
	attrs, err := ragit.DocumentAttributes(&docs[0])
	require.NoError(t, err)
	require.Equal(t, ragit.Attributes{"kind": "recording"}, attrs)
}

// The denormalized copy on chunks does not self-heal, which is why changing
// attributes goes through SetDocumentAttributes rather than an UPDATE.
func TestSetDocumentAttributes_ResyncsChunks(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	ctx := context.Background()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "a.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"course": "old"},
		Data:       []byte("Postgres in the original course."),
	})

	require.NoError(t, h.processor.SetDocumentAttributes(ctx, tenantID, doc.ID,
		ragit.Attributes{"course": "new"}))

	moved, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "new"},
	})
	require.NoError(t, err)
	require.Len(t, moved, 1, "the chunks' copy must move with the document")

	stale, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "old"},
	})
	require.NoError(t, err)
	require.Empty(t, stale, "chunks must not keep matching the labels they used to carry")

	// The document row moved too, so the catalog agrees with retrieval.
	docs, err := h.processor.ListDocuments(ctx, ragit.Tenant(tenantID), ragit.ListFilter{
		Attributes: ragit.Attributes{"course": "new"},
	})
	require.NoError(t, err)
	require.Len(t, docs, 1)
}

// Attributes narrow inside a scope; they never widen out of one.
func TestAttributes_DoNotCrossScope(t *testing.T) {
	h := newHarness(t, "acme")
	tenantA, tenantB := uuid.New(), uuid.New()
	ctx := context.Background()

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantA, Filename: "a.md", MimeType: "text/markdown",
		Attributes: ragit.Attributes{"course": "shared-label"},
		Data:       []byte("Postgres for tenant A."),
	})

	// The label matches, the tenant does not. Confinement wins.
	results, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantB), "postgres", ragit.SearchOptions{
		TopK: 10, Attributes: ragit.Attributes{"course": "shared-label"},
	})
	require.NoError(t, err)
	require.Empty(t, results)
}
