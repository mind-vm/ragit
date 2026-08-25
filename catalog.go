package ragit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"
)

// DefaultListLimit bounds a ListDocuments call that does not set one.
const DefaultListLimit = 50

// ListFilter narrows a catalog listing. Confinement is the [Scope] argument,
// not a field here: a catalog read is as much a boundary as a retrieval, and
// the same rule applies — the zero value must not widen anything.
type ListFilter struct {
	// Status restricts to documents in the given states. Empty means every
	// state, which is the useful default for "what has been uploaded".
	Status []string
	// Attributes restricts to documents carrying all of these key/value
	// pairs. Empty narrows nothing — like Status, and unlike Scope, this is a
	// filter rather than a boundary. See [Attributes].
	Attributes Attributes
	// Limit caps the result. Zero means DefaultListLimit.
	Limit int
	// Offset pages through results.
	Offset int
}

// preds renders the confinement and the narrowing filters together. The two
// are built in one place but mean different things: scope.preds() is the
// boundary, the rest narrows inside it.
func (f ListFilter) preds(scope Scope) ([]sqlb.Pred, error) {
	preds := scope.preds()
	if len(f.Status) > 0 {
		preds = append(preds, DocumentCols.Status.OneOf(f.Status...))
	}
	attrPred, ok, err := f.Attributes.containsPred("attributes")
	if err != nil {
		return nil, err
	}
	if ok {
		preds = append(preds, attrPred)
	}
	return preds, nil
}

// GetDocument reads one document by id, confined to scope.
//
// A document that exists but is outside the scope returns [ErrNotFound], the
// same as one that does not exist. Distinguishing the two would tell a caller
// that a document id is real and belongs to someone else, which is itself a
// disclosure.
func (p *Processor) GetDocument(ctx context.Context, scope Scope, documentID uuid.UUID) (*Document, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}

	var doc Document
	err := WithTenant(ctx, p.pool, scope.TenantID(), func(db sqlb.Executor) error {
		found, err := sqlb.Query[Document]().
			Where(append(scope.preds(), DocumentCols.ID.Eq(documentID))...).
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrNotFound, documentID)
		}
		doc = found
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

// ListDocuments returns the documents visible to scope, newest first.
//
// This is the catalog read a host application needs to answer "what has been
// indexed here", "is this upload still processing", and "why did it fail" —
// the last of which is why [Document.Error] is on the returned row rather than
// being reachable only through a failing call.
func (p *Processor) ListDocuments(ctx context.Context, scope Scope, filter ListFilter) ([]Document, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}

	preds, err := filter.preds(scope)
	if err != nil {
		return nil, err
	}

	var out []Document
	err = WithTenant(ctx, p.pool, scope.TenantID(), func(db sqlb.Executor) error {
		rows, err := sqlb.Query[Document]().
			Where(preds...).
			OrderBy(DocumentCols.CreatedAt.Desc(), DocumentCols.ID.Desc()).
			Limit(limit).
			Offset(filter.Offset).
			All(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: list documents: %w", err)
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CountDocuments returns how many documents match, ignoring paging.
func (p *Processor) CountDocuments(ctx context.Context, scope Scope, filter ListFilter) (int64, error) {
	if err := scope.Validate(); err != nil {
		return 0, err
	}

	preds, err := filter.preds(scope)
	if err != nil {
		return 0, err
	}

	var count int64
	err = WithTenant(ctx, p.pool, scope.TenantID(), func(db sqlb.Executor) error {
		n, err := sqlb.Query[Document]().Where(preds...).Count(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: count documents: %w", err)
		}
		count = n
		return nil
	})
	return count, err
}

// ListChunks returns a document's chunks in order, confined to scope. Useful
// for showing what was indexed, and for debugging a chunker change.
func (p *Processor) ListChunks(ctx context.Context, scope Scope, documentID uuid.UUID) ([]Chunk, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}

	var out []Chunk
	err := WithTenant(ctx, p.pool, scope.TenantID(), func(db sqlb.Executor) error {
		rows, err := sqlb.Query[Chunk]().
			Where(append(scope.preds(), ChunkCols.DocumentID.Eq(documentID))...).
			OrderBy(ChunkCols.ChunkIndex.Asc()).
			All(ctx, db)
		if err != nil {
			return fmt.Errorf("ragit: list chunks: %w", err)
		}
		out = rows
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
