package jobs

import (
	"context"

	"github.com/riverqueue/river"

	"github.com/jryannel/ragit"
)

// DeleteDocumentWorker runs Processor.DeleteDocument as a River job.
type DeleteDocumentWorker struct {
	river.WorkerDefaults[DeleteDocumentArgs]
	processor *ragit.Processor
}

// NewDeleteDocumentWorker builds a worker around an existing Processor.
func NewDeleteDocumentWorker(processor *ragit.Processor) *DeleteDocumentWorker {
	return &DeleteDocumentWorker{processor: processor}
}

func (w *DeleteDocumentWorker) Work(ctx context.Context, job *river.Job[DeleteDocumentArgs]) error {
	return w.processor.DeleteDocument(ctx, job.Args.DocumentID, job.Args.TenantID)
}
