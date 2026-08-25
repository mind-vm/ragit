package ragit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/chunk"
	"github.com/mind-vm/ragit/embed"
	"github.com/mind-vm/ragit/extract"
	"github.com/mind-vm/ragit/store"
)

type recordingSink struct {
	mu     sync.Mutex
	events []ragit.Event
}

func (r *recordingSink) DocumentProcessed(_ context.Context, e ragit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingSink) all() []ragit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ragit.Event(nil), r.events...)
}

func TestEventSink_FiresOnSuccess(t *testing.T) {
	h := newHarness(t, "acme")
	sink := &recordingSink{}
	h.processor.WithEventSink(sink)

	tenantID := uuid.New()
	scopeA := uuid.New()
	doc := h.ingest(t, ragit.DocumentInput{
		TenantID: tenantID, ScopeA: &scopeA,
		Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})

	events := sink.all()
	require.Len(t, events, 1)
	require.Equal(t, doc.ID, events[0].DocumentID)
	require.Equal(t, tenantID, events[0].TenantID)
	require.Equal(t, "db.md", events[0].Filename)
	require.Equal(t, ragit.StatusReady, events[0].Status)
	require.True(t, events[0].Succeeded())
	require.NotNil(t, events[0].ScopeA)
	require.Equal(t, scopeA, *events[0].ScopeA,
		"a subscriber routing a notification needs the scope, not just the tenant")
}

// The half a success-only callback would drop. A document that failed is
// exactly the case the person who uploaded it needs told about.
func TestEventSink_FiresOnTerminalFailure(t *testing.T) {
	pool := newHarnessPool(t)

	extractServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error_type":"ParsingError","message":"corrupt file"}`))
	}))
	defer extractServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{APIKey: "unused"})
	require.NoError(t, err)

	sink := &recordingSink{}
	processor := ragit.New(pool, extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.DefaultConfig()), embedder, store.NewMemoryStore()).
		WithEventSink(sink)

	tenantID := uuid.New()
	_, err = processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "broken.pdf", MimeType: "application/pdf",
		Data: []byte("not a pdf"),
	})
	require.Error(t, err)

	events := sink.all()
	require.Len(t, events, 1)
	require.Equal(t, ragit.StatusError, events[0].Status)
	require.False(t, events[0].Succeeded())
	require.Contains(t, events[0].Error, "corrupt file",
		"the subscriber needs the reason, not just the fact of failure")
}

func TestEventSink_FiresOnSkippedTooLarge(t *testing.T) {
	pool := newHarnessPool(t)

	extractServer := newExtractServer(t, longFixture)
	defer extractServer.Close()
	embedServer, _ := newEmbedServer(t, 1536, 0)
	defer embedServer.Close()

	embedder, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey: "k", BaseURL: embedServer.URL, Dimension: 1536,
	})
	require.NoError(t, err)

	sink := &recordingSink{}
	processor := ragit.New(pool, extract.NewXbergExtractor(extractServer.URL, 0),
		chunk.New(chunk.Config{Size: 100, Overlap: 10}), embedder, store.NewMemoryStore()).
		WithMaxChunksPerDocument(3).
		WithEventSink(sink)

	tenantID := uuid.New()
	_, err = processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "big.md", MimeType: "text/markdown",
		Data: []byte(longFixture),
	})
	require.NoError(t, err)

	events := sink.all()
	require.Len(t, events, 1)
	require.Equal(t, ragit.StatusSkippedTooLarge, events[0].Status)
	require.Contains(t, events[0].Error, "exceeding")
}

// A subscriber is an observer, not a participant: its failure must not undo
// indexing that already happened and was already paid for.
func TestEventSink_PanicDoesNotFailTheIndexing(t *testing.T) {
	h := newHarness(t, "acme")
	h.processor.WithEventSink(ragit.EventSinkFunc(func(context.Context, ragit.Event) {
		panic("subscriber blew up")
	}))

	tenantID := uuid.New()
	doc, err := h.processor.Ingest(context.Background(), ragit.DocumentInput{
		TenantID: tenantID, Filename: "db.md", MimeType: "text/markdown",
		Data: []byte("Postgres stores relational data durably."),
	})
	require.NoError(t, err)
	require.Equal(t, ragit.StatusReady, doc.Status)

	results, err := h.processor.VectorSearch(context.Background(),
		ragit.Tenant(tenantID), "postgres", ragit.SearchOptions{TopK: 5})
	require.NoError(t, err)
	require.NotEmpty(t, results, "the chunks are indexed regardless of the subscriber")
}
