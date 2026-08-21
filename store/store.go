// Package store puts and gets original document bytes in object storage.
package store

import (
	"context"
	"io"

	"github.com/google/uuid"
)

// Store puts and gets raw document bytes, keyed by a tenant-scoped URI.
type Store interface {
	// Put uploads data under a tenant-prefixed key and returns the URI it
	// was stored at.
	Put(ctx context.Context, tenantID uuid.UUID, filename string, data []byte, mimeType string) (uri string, err error)
	// Get retrieves the object at uri. The caller must close the reader.
	Get(ctx context.Context, uri string) (io.ReadCloser, error)
}
