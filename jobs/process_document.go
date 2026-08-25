package jobs

import (
	"context"
	"errors"

	"github.com/riverqueue/river"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/embed"
	"github.com/mind-vm/ragit/extract"
)

// ProcessDocumentWorker runs Processor.ProcessDocument as a River job.
type ProcessDocumentWorker struct {
	river.WorkerDefaults[ProcessDocumentArgs]
	processor *ragit.Processor
}

// NewProcessDocumentWorker builds a worker around an existing Processor.
func NewProcessDocumentWorker(processor *ragit.Processor) *ProcessDocumentWorker {
	return &ProcessDocumentWorker{processor: processor}
}

// Work classifies ProcessDocument's error per docs/design.md §7: a
// transport/deployment failure (extract or embed unavailable) is retried
// with River's normal backoff; a rate limit gets a longer, deliberate
// snooze; anything else is a verdict on the document and is not retried.
func (w *ProcessDocumentWorker) Work(ctx context.Context, job *river.Job[ProcessDocumentArgs]) error {
	err := w.processor.ProcessDocument(ctx, job.Args.DocumentID, job.Args.TenantID)
	return classify(err)
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, extract.ErrUnavailable), errors.Is(err, embed.ErrUnavailable):
		return err // transient — River's normal retry/backoff
	case errors.Is(err, embed.ErrRateLimited):
		return river.JobSnooze(RateLimitBackoff)
	default:
		return river.JobCancel(err) // a verdict on the document — don't retry
	}
}
