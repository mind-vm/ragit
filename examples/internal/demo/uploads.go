package demo

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/examples/fixtures"
	"github.com/jryannel/ragit/examples/internal/bootstrap"
)

// EnsureDocument returns the ragit document for a fixture, creating it only if
// this application has not already uploaded that filename for that tenant.
//
// The dedupe is the *application's*, keyed off its own demo_uploads table, and
// that is the point. ragit has no opinion about whether two uploads of the same
// filename are the same document — it cannot, since it does not know what a
// document means to the application. CreateDocument called twice writes two
// documents and embeds both.
//
// It also makes re-running an example free: the second run finds every
// document already present and ProcessDocument resumes rather than re-billing
// the embedding provider.
func EnsureDocument(
	ctx context.Context,
	pool *pgxpool.Pool,
	processor *ragit.Processor,
	tenantID uuid.UUID,
	doc fixtures.Doc,
) (id uuid.UUID, created bool, err error) {
	existing, err := sqlb.Query[bootstrap.Upload]().
		Where(sqlb.RawPred("tenant_id = ? AND filename = ?", tenantID, doc.Filename)).
		One(ctx, pool)
	if err == nil && existing.DocumentID != nil {
		return *existing.DocumentID, false, nil
	}

	attributes := make(ragit.Attributes, len(doc.Attributes))
	for k, v := range doc.Attributes {
		attributes[k] = v
	}

	id, err = processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID:   tenantID,
		Filename:   doc.Filename,
		MimeType:   doc.MimeType,
		Data:       doc.Data,
		Attributes: attributes,
	})
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("create %s: %w", doc.Filename, err)
	}

	upload := &bootstrap.Upload{
		TenantID:   tenantID,
		DocumentID: &id,
		Filename:   doc.Filename,
		UploadedBy: "examples",
	}
	if _, err := sqlb.InsertRows(upload).Omit("id", "created_at", "updated_at").One(ctx, pool); err != nil {
		return uuid.Nil, false, fmt.Errorf("record upload of %s: %w", doc.Filename, err)
	}
	return id, true, nil
}
