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
		return nil, fmt.Errorf("store: no object at %s", uri)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
