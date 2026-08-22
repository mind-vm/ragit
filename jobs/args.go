// Package jobs wires ragit's Processor into a River job queue.
//
// ragit does not own a river.Client — a host app builds and starts its own
// (often with a direct, non-pooled Postgres connection, since River's
// LISTEN/NOTIFY needs one; that's a host-app concern documented here, not
// something this package can enforce) and calls river.AddWorker with the
// workers this package constructs. This mirrors the "Registration"
// contribution pattern the reference implementation (valiro-go) uses to let
// a subsystem add workers/queues to a shared client without the platform
// layer importing the subsystem — simplified here since a standalone
// library has no such platform layer to invert around.
package jobs

import (
	"time"

	"github.com/riverqueue/river"
)

// RateLimitBackoff is the snooze duration used when the embedding provider
// rate-limits a batch (embed.ErrRateLimited). A longer, deliberate wait
// rather than River's normal exponential backoff.
const RateLimitBackoff = 5 * time.Minute

// ProcessDocumentArgs identifies which document to process. Kind and Queue
// are namespaced ("ragit_...") to avoid colliding with a host app's own job
// kinds/queues in the same River client.
type ProcessDocumentArgs struct {
	DocumentID string `json:"document_id"`
	TenantID   string `json:"tenant_id"`
}

func (ProcessDocumentArgs) Kind() string { return "ragit_process_document" }

func (ProcessDocumentArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "ragit_process_document", MaxAttempts: 5}
}

// DeleteDocumentArgs identifies which document to delete.
type DeleteDocumentArgs struct {
	DocumentID string `json:"document_id"`
	TenantID   string `json:"tenant_id"`
}

func (DeleteDocumentArgs) Kind() string { return "ragit_delete_document" }

func (DeleteDocumentArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: "ragit_process_document", MaxAttempts: 5}
}
