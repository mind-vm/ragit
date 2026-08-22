// Package store puts and gets original document bytes in object storage.
package store

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get and Delete when no object exists at a URI.
// Delete implementations treat it as success — deleting an object that is
// already gone is the outcome the caller asked for — but it is exported so
// callers distinguishing "never stored" from "storage broke" can.
var ErrNotFound = errors.New("store: object not found")

// Store puts and gets raw document bytes, keyed by a tenant-scoped URI.
type Store interface {
	// Put uploads data under a tenant-prefixed key and returns the URI it
	// was stored at.
	Put(ctx context.Context, tenantID string, filename string, data []byte, mimeType string) (uri string, err error)
	// Get retrieves the object at uri. The caller must close the reader.
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
	// Delete removes the object at uri. It is idempotent: deleting a URI
	// that holds no object returns nil, so a retried cleanup does not fail
	// on its second pass.
	Delete(ctx context.Context, uri string) error
}
