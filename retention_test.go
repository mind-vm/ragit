package ragit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
)

func TestDeleteDocument_PurgesStoredBytes(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	require.Equal(t, 1, h.store.Len())

	ctx := context.Background()
	require.NoError(t, h.processor.DeleteDocument(ctx, tenantID, doc.ID))
	require.Zero(t, h.store.Len(), "the bytes must go with the row, not linger in object storage")

	results, err := h.processor.VectorSearch(ctx, ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, results, "chunks cascade with the document")
}

func TestDeleteExpired_RemovesOnlyExpiredDocuments(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	sessionID := uuid.New()

	past := time.Now().Add(-time.Hour)
	future := time.Now().Add(time.Hour)

	expired := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, SessionID: &sessionID, ExpiresAt: &past,
		Filename: "attachment.md", MimeType: "text/markdown",
		Data: []byte("Postgres attachment from a finished conversation."),
	})
	notYet := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, SessionID: &sessionID, ExpiresAt: &future,
		Filename: "live.md", MimeType: "text/markdown",
		Data: []byte("Postgres attachment from a live conversation."),
	})
	durable := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "library.md", MimeType: "text/markdown",
		Data: []byte("Postgres in the durable library."),
	})
	require.Equal(t, 3, h.store.Len())

	ctx := context.Background()
	result, err := h.processor.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, result.Documents)
	require.Empty(t, result.ObjectErrors)
	require.Equal(t, 2, h.store.Len())

	sessionScope := ragit.Tenant(tenantID).Session(sessionID)
	_, err = h.processor.GetDocument(ctx, sessionScope, expired.ID)
	require.ErrorIs(t, err, ragit.ErrNotFound)

	// A clock that has not run out, and a document with no clock at all, are
	// both untouched — "expires_at IS NULL" must never mean "expired".
	live, err := h.processor.GetDocument(ctx, sessionScope, notYet.ID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, live.Status)

	kept, err := h.processor.GetDocument(ctx, ragit.Tenant(tenantID), durable.ID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, kept.Status)
}

func TestDeleteExpired_SweepsAcrossTenants(t *testing.T) {
	h := newHarness(t, "acme")
	tenantA, tenantB := uuid.New(), uuid.New()
	past := time.Now().Add(-time.Hour)

	docA := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantA, ExpiresAt: &past,
		Filename: "a.md", MimeType: "text/markdown", Data: []byte("Postgres for tenant A."),
	})
	docB := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantB, ExpiresAt: &past,
		Filename: "b.md", MimeType: "text/markdown", Data: []byte("Postgres for tenant B."),
	})

	// The sweep cannot enumerate the tenants owning expired rows without first
	// reading across tenants, which is why it runs under the maintenance scope.
	ctx := context.Background()
	result, err := h.processor.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, result.Documents)

	_, err = h.processor.GetDocument(ctx, ragit.Tenant(tenantA), docA.ID)
	require.ErrorIs(t, err, ragit.ErrNotFound)
	_, err = h.processor.GetDocument(ctx, ragit.Tenant(tenantB), docB.ID)
	require.ErrorIs(t, err, ragit.ErrNotFound)
	require.Zero(t, h.store.Len())
}

func TestDeleteExpired_IsIdempotent(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()
	past := time.Now().Add(-time.Hour)

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ExpiresAt: &past,
		Filename: "a.md", MimeType: "text/markdown", Data: []byte("Postgres."),
	})

	ctx := context.Background()
	first, err := h.processor.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, first.Documents)

	// A scheduled sweep runs constantly against a corpus that is usually
	// clean; the empty case must be cheap and silent, not an error.
	second, err := h.processor.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Zero(t, second.Documents)
	require.Zero(t, second.Chunks)
	require.Empty(t, second.ObjectErrors)
}

// Chunks carry their own retention clock, copied from the document at
// processing time. This sweeps the case the FK cascade cannot: a chunk whose
// clock has run out while its document's has not.
func TestDeleteExpired_RemovesExpiredChunksWhoseDocumentLives(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "library.md", MimeType: "text/markdown",
		Data: []byte("Postgres in the durable library."),
	})

	// The UPDATE runs inside the tenant transaction: issued straight at the
	// pool it would match zero rows, because RLS is doing its job.
	ctx := context.Background()
	require.NoError(t, ragit.WithTenant(ctx, h.pool, tenantID, func(db sqlb.Executor) error {
		tag, err := db.Exec(ctx,
			"UPDATE ragit_chunks SET expires_at = now() - interval '1 hour' WHERE document_id = $1", doc.ID)
		require.NoError(t, err)
		require.Positive(t, tag.RowsAffected(), "test setup affected no rows")
		return nil
	}))

	result, err := h.processor.DeleteExpired(ctx)
	require.NoError(t, err)
	require.Zero(t, result.Documents, "the document has no clock and must survive")
	require.Positive(t, result.Chunks)

	kept, err := h.processor.GetDocument(ctx, ragit.Tenant(tenantID), doc.ID)
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, kept.Status)

	chunks, err := h.processor.ListChunks(ctx, ragit.Tenant(tenantID), doc.ID)
	require.NoError(t, err)
	require.Empty(t, chunks)
	require.Equal(t, 1, h.store.Len(), "the surviving document keeps its bytes")
}

// The maintenance escape widens reads and deletes only. The policies' WITH
// CHECK clause stays tenant-scoped precisely so no maintenance path can write
// a row into a tenant it is not scoped to.
func TestMaintenanceScope_CannotWriteAcrossTenants(t *testing.T) {
	pool := newHarnessPool(t)
	ctx := context.Background()

	err := ragit.WithMaintenance(ctx, pool, func(db sqlb.Executor) error {
		_, err := db.Exec(ctx,
			"INSERT INTO ragit_documents (tenant_id, filename, mime_type) VALUES ($1, $2, $3)",
			uuid.New(), "smuggled.md", "text/markdown")
		return err
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "row-level security")
}

// TestRLS_FailsClosedWithoutTenantScope pins down the property the WithTenant
// plumbing exists for. It is deliberately a database-level test: the risk is a
// future query issued outside WithTenant, which no API-level test would catch.
func TestRLS_FailsClosedWithoutTenantScope(t *testing.T) {
	h := newHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	ctx := context.Background()
	scoped, err := h.processor.ListChunks(ctx, ragit.Tenant(tenantID), doc.ID)
	require.NoError(t, err)
	require.NotEmpty(t, scoped)

	// Outside a tenant transaction — the GUC never set — the policy evaluates
	// to NULL and the rows vanish rather than being exposed.
	var docs, chunks int
	require.NoError(t, h.pool.QueryRow(ctx, "SELECT count(*) FROM ragit_documents").Scan(&docs))
	require.NoError(t, h.pool.QueryRow(ctx, "SELECT count(*) FROM ragit_chunks").Scan(&chunks))
	require.Zero(t, docs, "RLS must fail closed when no tenant scope is set")
	require.Zero(t, chunks)
}
