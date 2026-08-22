package ragit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jryannel/sqlb"
)

// DeleteExpiredBatchSize bounds one retention sweep pass, so a large backlog
// is worked through over several runs instead of one enormous transaction.
const DeleteExpiredBatchSize = 500

// RetentionResult reports what one DeleteExpired pass removed.
type RetentionResult struct {
	Documents int
	Chunks    int
	// ObjectErrors holds failures to purge object storage. They do not fail
	// the sweep: the rows are already gone, so a later pass will never revisit
	// these objects, and surfacing them here is the only way a caller learns
	// about the orphans.
	ObjectErrors []error
}

// DeleteExpired removes documents and chunks whose retention clock has run
// out, across every tenant, along with their stored bytes.
//
// It is cross-tenant, which is why it runs under [WithMaintenance] rather than
// a tenant scope — finding expired rows means reading rows whose owning
// tenants cannot be enumerated beforehand, and enumerating them would itself
// be the cross-tenant read. It processes at most DeleteExpiredBatchSize
// documents per call and is safe to run on a schedule; see the jobs package
// for a River worker.
func (p *Processor) DeleteExpired(ctx context.Context) (*RetentionResult, error) {
	result := &RetentionResult{}
	now := time.Now()

	var expired []Document
	if err := WithMaintenance(ctx, p.pool, func(db sqlb.Executor) error {
		rows, err := sqlb.Query[Document]().
			Where(DocumentCols.ExpiresAt.NotNull(), DocumentCols.ExpiresAt.Lte(now)).
			OrderBy(DocumentCols.ExpiresAt.Asc()).
			Limit(DeleteExpiredBatchSize).
			All(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: list expired documents: %w", err)
		}
		expired = rows
		if len(expired) == 0 {
			return nil
		}

		ids := make([]uuid.UUID, len(expired))
		for i, d := range expired {
			ids[i] = d.ID
		}
		deleted, err := sqlb.DeleteRows[Document]().
			Where(DocumentCols.ID.OneOf(ids...)).
			Exec(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: delete expired documents: %w", err)
		}
		result.Documents = int(deleted)
		return nil
	}); err != nil {
		return nil, err
	}

	// Chunk-only expiries are swept after the document pass, so this sees only
	// chunks whose own clock ran out rather than ones the FK cascade already
	// removed.
	if err := WithMaintenance(ctx, p.pool, func(db sqlb.Executor) error {
		deleted, err := sqlb.DeleteRows[Chunk]().
			Where(ChunkCols.ExpiresAt.NotNull(), ChunkCols.ExpiresAt.Lte(now)).
			Exec(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: delete expired chunks: %w", err)
		}
		result.Chunks = int(deleted)
		return nil
	}); err != nil {
		return nil, err
	}

	// Object storage is purged only after the rows are committed gone, so a
	// crash mid-sweep leaves orphaned objects rather than rows pointing at
	// bytes that no longer exist.
	for _, d := range expired {
		if d.SourceURI == nil {
			continue
		}
		if err := p.store.Delete(ctx, *d.SourceURI); err != nil {
			result.ObjectErrors = append(result.ObjectErrors, fmt.Errorf("purge %s: %w", *d.SourceURI, err))
		}
	}
	return result, nil
}
