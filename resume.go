package ragit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"
)

// ResumeChunks reports which of a document's chunks are already persisted in
// the embedding space named by fingerprint, so a caller embeds only what is
// missing instead of paying for the whole document again.
//
// contents is the document's freshly chunked text, positionally: contents[i]
// is the content of chunk index i. The returned slice is parallel to it, and
// true means that chunk is already stored under fingerprint with byte-
// identical content — skip it.
//
// A stored chunk survives only if its fingerprint matches AND its content
// matches contents at the same index. On the first disagreement — a different
// embedder, re-chunked text, an index past the end of the fresh set — every
// chunk for the document is deleted and the result is all false. That is
// deliberate and total: two embedding spaces inside one document produce
// cosine distances that are not a weaker signal but a meaningless one, and a
// partial wipe is exactly how that state arises.
//
// It runs on the caller's executor rather than opening its own transaction, so
// a caller writing chunks by hand — the sqlb escape hatch this package's doc
// comment describes — can put the guard and its own inserts in one
// transaction, and needs no [Processor] to reach it:
//
//	err := ragit.WithTenant(ctx, pool, tenantID, func(db sqlb.Executor) error {
//		reusable, err := ragit.ResumeChunks(ctx, db, tenantID, docID, contents, fp)
//		if err != nil {
//			return err
//		}
//		// ... embed and insert every index whose entry is false ...
//	})
//
// [Processor.ProcessDocument] calls it too. This is the guard itself, not a
// second copy of the rule.
func ResumeChunks(ctx context.Context, db sqlb.Executor, tenantID, documentID uuid.UUID, contents []string, fingerprint string) ([]bool, error) {
	stored, err := sqlb.Query[Chunk]().
		Where(ChunkCols.DocumentID.Eq(documentID), ChunkCols.TenantID.Eq(tenantID)).
		OrderBy(ChunkCols.ChunkIndex.Asc()).
		All(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("ragit: load existing chunks: %w", err)
	}

	reusable := make([]bool, len(contents))
	for _, s := range stored {
		i := int(s.ChunkIndex)
		fresh := i >= 0 && i < len(contents) &&
			s.EmbeddingFingerprint != nil && *s.EmbeddingFingerprint == fingerprint &&
			s.Content == contents[i]
		if !fresh {
			if err := deleteChunks(ctx, db, tenantID, documentID); err != nil {
				return nil, err
			}
			return make([]bool, len(contents)), nil
		}
		reusable[i] = true
	}
	return reusable, nil
}

// deleteChunks removes every chunk of a document on the caller's executor.
func deleteChunks(ctx context.Context, db sqlb.Executor, tenantID, documentID uuid.UUID) error {
	if _, err := sqlb.DeleteRows[Chunk]().
		Where(ChunkCols.DocumentID.Eq(documentID), ChunkCols.TenantID.Eq(tenantID)).
		Exec(ctx, db); err != nil {
		return fmt.Errorf("ragit: clear chunks: %w", err)
	}
	return nil
}
