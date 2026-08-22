package ragit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Event reports that a document reached a terminal state.
type Event struct {
	DocumentID uuid.UUID
	TenantID   uuid.UUID
	ScopeA     *uuid.UUID
	ScopeB     *uuid.UUID
	SessionID  *uuid.UUID
	Filename   string
	// Status is one of StatusReady, StatusError or StatusSkippedTooLarge.
	Status string
	// Error carries the failure message for StatusError and the reason for
	// StatusSkippedTooLarge. Empty for StatusReady.
	Error string
	// ChunkCount is the number of chunks indexed. Zero unless Status is
	// StatusReady.
	ChunkCount int
	At         time.Time
}

// Succeeded reports whether the document is now searchable.
func (e Event) Succeeded() bool { return e.Status == StatusReady }

// EventSink observes documents reaching a terminal state.
//
// Two properties this contract commits to, because both matter to what a
// subscriber can be built on:
//
// It fires on **every** terminal state, not only success. A document that
// ended in error or was skipped as too large is precisely the case a user who
// uploaded it needs told about, so a success-only callback would leave the
// interesting half unreported. Check [Event.Succeeded].
//
// It fires **after** the chunks are committed, and its error is ignored. A
// subscriber that fails must not roll back or retry the indexing: the indexing
// already happened, the work is already paid for, and re-running it would
// re-bill the embedding provider to satisfy a notification. Handle and log
// failures inside the sink.
//
// A sink that blocks holds up the job that called it, so a slow subscriber
// should hand off to its own queue.
//
// If a transactional guarantee is wanted later, a durable outbox table written
// in the same transaction as the chunks is the shape to reach for; this
// interface is the seam it would be implemented behind.
type EventSink interface {
	DocumentProcessed(ctx context.Context, event Event)
}

// EventSinkFunc adapts a function to [EventSink].
type EventSinkFunc func(ctx context.Context, event Event)

// DocumentProcessed implements [EventSink].
func (f EventSinkFunc) DocumentProcessed(ctx context.Context, event Event) { f(ctx, event) }

// publish notifies the sink, if one is attached.
//
// A panicking subscriber must not take down the worker that indexed the
// document — the indexing succeeded, and losing that outcome to a bug in a
// notification handler would turn an observer into a single point of failure.
func (p *Processor) publish(ctx context.Context, doc *Document, status, errMsg string) {
	if p.sink == nil {
		return
	}
	event := Event{
		DocumentID: doc.ID,
		TenantID:   doc.TenantID,
		ScopeA:     doc.ScopeAID,
		ScopeB:     doc.ScopeBID,
		SessionID:  doc.SessionID,
		Filename:   doc.Filename,
		Status:     status,
		Error:      errMsg,
		At:         time.Now(),
	}
	if doc.ChunkCount != nil {
		event.ChunkCount = int(*doc.ChunkCount)
	}

	defer func() { _ = recover() }()
	p.sink.DocumentProcessed(ctx, event)
}
