package ragit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"
)

// Attributes are the host application's own key/value pairs on a document.
//
// ragit stores and filters them without interpreting them: they are the seam
// for narrowing a search by facts ragit does not model — a course id, a
// language, a document kind, a visibility label the application understands.
//
// They are kept separate from Document.Metadata, which holds whatever the
// extractor produced (page count, detected language, table warnings). Merging
// the two would let a new xberg field collide with an application key.
//
// # Attributes are not a security boundary
//
// [Scope] is. An attribute filter narrows a result set that confinement has
// already bounded, and an *empty* filter narrows nothing — the opposite of
// Scope's rule, and deliberately so, because a forgotten attribute filter
// should return more rows rather than none.
//
// So do not use attributes for access control. A caller that must not see a
// document should be outside its scope, not merely failing to match a label;
// otherwise the day someone forgets the filter is the day the document leaks.
type Attributes map[string]string

// containsPred renders a JSONB containment predicate over the given column.
// Containment (`@>`) rather than equality, so a filter names the pairs it
// cares about and ignores the rest.
func (a Attributes) containsPred(column string) (sqlb.Pred, bool, error) {
	if len(a) == 0 {
		return sqlb.Pred{}, false, nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return sqlb.Pred{}, false, fmt.Errorf("ragit: encode attribute filter: %w", err)
	}
	return sqlb.RawPred(column+" @> ?::jsonb", string(encoded)), true, nil
}

// raw encodes attributes for storage, normalising nil to an empty object so
// the column is never NULL.
func (a Attributes) raw() (json.RawMessage, error) {
	if len(a) == 0 {
		return json.RawMessage("{}"), nil
	}
	encoded, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("ragit: encode attributes: %w", err)
	}
	return encoded, nil
}

// DocumentAttributes decodes a document's stored attributes.
func DocumentAttributes(doc *Document) (Attributes, error) {
	if len(doc.Attributes) == 0 {
		return Attributes{}, nil
	}
	var out Attributes
	if err := json.Unmarshal(doc.Attributes, &out); err != nil {
		return nil, fmt.Errorf("ragit: decode attributes: %w", err)
	}
	return out, nil
}

// SetDocumentAttributes replaces a document's attributes and re-stamps its
// chunks to match.
//
// The resync is why this exists rather than callers updating the row: chunks
// carry a denormalized copy so retrieval can filter without a join, and that
// copy does not self-heal. Reprocessing will not fix it either — the resume
// check sees identical content and skips the rewrite, leaving chunks matching
// the labels they used to have. Same obligation as
// [Processor.MoveDocumentScope], for the same reason.
func (p *Processor) SetDocumentAttributes(ctx context.Context, tenantID, documentID uuid.UUID, attrs Attributes) error {
	encoded, err := attrs.raw()
	if err != nil {
		return err
	}

	return WithTenant(ctx, p.pool, tenantID, func(db sqlb.Executor) error {
		rows, err := sqlb.UpdateRows[Document]().
			Set("attributes", encoded).
			Set("updated_at", nowFunc()).
			Where(DocumentCols.ID.Eq(documentID), DocumentCols.TenantID.Eq(tenantID)).
			Exec(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: update document attributes: %w", err)
		}
		if len(rows) == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, documentID)
		}

		if _, err := sqlb.UpdateRows[Chunk]().
			Set("attributes", encoded).
			Where(ChunkCols.DocumentID.Eq(documentID), ChunkCols.TenantID.Eq(tenantID)).
			Exec(ctx, db); err != nil {
			return fmt.Errorf("ragit: resync chunk attributes: %w", err)
		}
		return nil
	})
}
