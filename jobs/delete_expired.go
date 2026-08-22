package jobs

import (
	"context"
	"time"

	"github.com/riverqueue/river"

	"github.com/jryannel/ragit"
)

// DeleteExpiredArgs carries no state: the sweep finds its own work. It is
// meant to be scheduled periodically rather than enqueued per document.
//
// Wire it up with River's periodic jobs, e.g.
//
//	river.NewPeriodicJob(
//	    river.PeriodicInterval(15*time.Minute),
//	    func() (river.JobArgs, *river.InsertOpts) { return jobs.DeleteExpiredArgs{}, nil },
//	    &river.PeriodicJobOpts{RunOnStart: true},
//	)
type DeleteExpiredArgs struct{}

func (DeleteExpiredArgs) Kind() string { return "ragit_delete_expired" }

func (DeleteExpiredArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: "ragit_process_document",
		// One sweep in flight at a time. Two concurrent passes would race to
		// delete the same rows, and the loser would do a lot of work to
		// delete nothing. The args are an empty struct, so ByArgs makes every
		// instance identical; ByState is left unset to take River's default.
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
		MaxAttempts: 3,
	}
}

// DeleteExpiredWorker runs Processor.DeleteExpired as a River job, clearing
// documents and chunks whose retention clock has run out (design.md §8's
// ephemeral attachment scope).
type DeleteExpiredWorker struct {
	river.WorkerDefaults[DeleteExpiredArgs]
	processor *ragit.Processor
	// batchTimeout bounds one sweep pass.
	batchTimeout time.Duration
}

// DefaultRetentionSweepTimeout bounds a single retention pass. A pass is
// capped at ragit.DeleteExpiredBatchSize documents, so this is generous
// rather than tight.
const DefaultRetentionSweepTimeout = 5 * time.Minute

// NewDeleteExpiredWorker builds the retention sweep worker.
func NewDeleteExpiredWorker(processor *ragit.Processor) *DeleteExpiredWorker {
	return &DeleteExpiredWorker{processor: processor, batchTimeout: DefaultRetentionSweepTimeout}
}

// Timeout implements river.Worker.
func (w *DeleteExpiredWorker) Timeout(*river.Job[DeleteExpiredArgs]) time.Duration {
	return w.batchTimeout
}

// Work runs one sweep pass.
//
// A failure to purge object storage is reported but does not fail the job:
// the rows are already committed gone, so retrying the job would re-run the
// sweep against a corpus that no longer contains them and would never revisit
// the orphaned objects. Returning an error here would produce noisy retries
// that cannot fix anything. Orphans are a storage-lifecycle concern.
func (w *DeleteExpiredWorker) Work(ctx context.Context, job *river.Job[DeleteExpiredArgs]) error {
	result, err := w.processor.DeleteExpired(ctx)
	if err != nil {
		return err
	}
	output := map[string]any{
		"documents_deleted": result.Documents,
		"chunks_deleted":    result.Chunks,
	}
	if len(result.ObjectErrors) > 0 {
		output["object_errors"] = errorStrings(result.ObjectErrors)
	}
	// A failure to record output is not a reason to fail — and re-running the
	// sweep would not reproduce it, since the rows are already gone.
	_ = river.RecordOutput(ctx, output)
	return nil
}

func errorStrings(errs []error) []string {
	out := make([]string, len(errs))
	for i, err := range errs {
		out[i] = err.Error()
	}
	return out
}
