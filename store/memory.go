package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
)

// MemoryStore is an in-memory Store, for tests. Not safe to use as a
// production backend — nothing is persisted.
type MemoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

var _ Store = (*MemoryStore)(nil)

// NewMemoryStore builds an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{objects: make(map[string][]byte)}
}

func (s *MemoryStore) Put(_ context.Context, tenantID uuid.UUID, filename string, data []byte, _ string) (string, error) {
	uri := fmt.Sprintf("memory://%s/%s/%s", tenantID, uuid.NewString(), filename)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[uri] = append([]byte(nil), data...)
	return uri, nil
}

func (s *MemoryStore) Get(_ context.Context, uri string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[uri]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, uri)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// Delete removes the object, if present. Missing is not an error.
func (s *MemoryStore) Delete(_ context.Context, uri string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, uri)
	return nil
}

// Len reports how many objects are held, for tests asserting that a delete
// actually purged the bytes rather than only the database row.
func (s *MemoryStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.objects)
}
