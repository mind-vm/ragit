package ragit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/internal/db"
	"github.com/jryannel/ragit/internal/testutil"
	"github.com/jryannel/ragit/search"
)

func TestDeleteDocument_PurgesStoredBytes(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	require.Equal(t, 1, h.store.Len())

	require.NoError(t, h.processor.DeleteDocument(context.Background(), doc.ID, tenantID))

	require.Zero(t, h.store.Len(), "the original bytes must go with the row, not linger in object storage")

	results, err := h.processor.VectorSearch(context.Background(), tenantID, "postgres", search.Options{TopK: 5})
	require.NoError(t, err)
	require.Empty(t, results, "chunks cascade with the document")
}

func TestDeleteExpired_RemovesOnlyExpiredDocuments(t *testing.T) {
	h := newSearchHarness(t, "acme")
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

	result, err := h.processor.DeleteExpired(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, result.Documents)
	require.Empty(t, result.ObjectErrors)

	// The expired attachment is gone, bytes included.
	require.Equal(t, 2, h.store.Len())
	requireDocumentAbsent(t, h, tenantID, expired.ID)

	// A clock that has not run out, and a document with no clock at all, are
	// both untouched — "expires_at IS NULL" must never mean "expired".
	require.Equal(t, "ready", getDoc(t, h.pool, tenantID, notYet.ID).Status)
	require.Equal(t, "ready", getDoc(t, h.pool, tenantID, durable.ID).Status)
}

func TestDeleteExpired_SweepsAcrossTenants(t *testing.T) {
	h := newSearchHarness(t, "acme")
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

	// The sweep cannot enumerate the tenants owning expired rows without
	// first reading across tenants, which is why it runs under the
	// maintenance scope rather than a tenant scope.
	result, err := h.processor.DeleteExpired(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, result.Documents)

	requireDocumentAbsent(t, h, tenantA, docA.ID)
	requireDocumentAbsent(t, h, tenantB, docB.ID)
	require.Zero(t, h.store.Len())
}

func TestDeleteExpired_IsIdempotent(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()
	past := time.Now().Add(-time.Hour)

	h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ExpiresAt: &past,
		Filename: "a.md", MimeType: "text/markdown", Data: []byte("Postgres."),
	})

	first, err := h.processor.DeleteExpired(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, first.Documents)

	// A scheduled sweep runs constantly against a corpus that is usually
	// clean; the empty case must be cheap and silent, not an error.
	second, err := h.processor.DeleteExpired(context.Background())
	require.NoError(t, err)
	require.Zero(t, second.Documents)
	require.Zero(t, second.Chunks)
	require.Empty(t, second.ObjectErrors)
}

// Chunks carry their own retention clock, copied from the document at
// processing time. This sweeps the case the FK cascade cannot: a chunk whose
// clock has run out while its document's has not.
func TestDeleteExpired_RemovesExpiredChunksWhoseDocumentLives(t *testing.T) {
	h := newSearchHarness(t, "acme")
	tenantID := uuid.New()

	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, Filename: "library.md", MimeType: "text/markdown",
		Data: []byte("Postgres in the durable library."),
	})

	// Expire the chunks alone, leaving the document with no clock at all.
	// The UPDATE has to run inside the tenant transaction: issued straight at
	// the pool it would match zero rows, because RLS is doing its job.
	execInTenant(t, h.pool, tenantID,
		"UPDATE ragit_chunks SET expires_at = now() - interval '1 hour' WHERE document_id = $1", doc.ID)

	result, err := h.processor.DeleteExpired(context.Background())
	require.NoError(t, err)
	require.Zero(t, result.Documents, "the document has no clock and must survive")
	require.Positive(t, result.Chunks)

	require.Equal(t, "ready", getDoc(t, h.pool, tenantID, doc.ID).Status)
	require.Empty(t, getChunks(t, h.pool, tenantID, doc.ID))
	require.Equal(t, 1, h.store.Len(), "the surviving document keeps its bytes")
}

// The maintenance escape widens reads and deletes only. Migration 00003
// leaves WITH CHECK tenant-scoped precisely so no maintenance path can write
// a row into a tenant it is not scoped to.
func TestMaintenanceScope_CannotWriteAcrossTenants(t *testing.T) {
	pool := testutil.SetupTestPool(t)
	ctx := context.Background()

	err := db.WithMaintenance(ctx, pool, func(q *db.Queries) error {
		_, err := q.CreateDocument(ctx, db.CreateDocumentParams{
			TenantID: uuid.New(),
			Filename: "smuggled.md",
			MimeType: "text/markdown",
		})
		return err
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "row-level security")
}

// execInTenant runs raw SQL inside a tenant-scoped transaction, for test
// setup that has no sqlc query of its own.
func execInTenant(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, "SELECT set_config($1, $2, true)", db.TenantGUC, tenantID.String())
	require.NoError(t, err)

	tag, err := tx.Exec(ctx, sql, args...)
	require.NoError(t, err)
	require.Positive(t, tag.RowsAffected(), "test setup affected no rows")
	require.NoError(t, tx.Commit(ctx))
}

func requireDocumentAbsent(t *testing.T, h *searchHarness, tenantID, documentID uuid.UUID) {
	t.Helper()
	err := db.WithTenant(context.Background(), h.pool, tenantID, func(q *db.Queries) error {
		_, err := q.GetDocumentByID(context.Background(), db.GetDocumentByIDParams{
			ID: documentID, TenantID: tenantID,
		})
		return err
	})
	require.Error(t, err, "document %s should have been swept", documentID)
}
