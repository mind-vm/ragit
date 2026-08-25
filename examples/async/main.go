// Command async runs the extract-only pipeline the way a deployment actually
// would: nothing is processed inline, everything goes through River.
//
// It exists because ragit's jobs package had never been exercised from outside
// this module. extract-only calls CreateDocument and ProcessDocument back to
// back, which is honest about the seam but never proves the queue wiring works
// — that the workers register, that the error classification does what §7 says,
// that the retention sweep finds its own work.
//
// It is a third program rather than a flag on extract-only deliberately. The
// point of extract-only and xberg-owned is that they are readable side by side
// and differ only in pipeline shape; a River client, a subscription and a
// shutdown sequence in one of them would wreck that comparison. Queue wiring is
// an orthogonal concern and gets its own file.
//
//	cd examples && make up && make verify
//	export EDENAI_API_KEY=$(grep '^EDENAI_API_KEY=' ~/.config/envs/valiro-go.env | cut -d= -f2-)
//	go run ./async
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/examples/fixtures"
	"github.com/jryannel/ragit/examples/internal/bootstrap"
	"github.com/jryannel/ragit/examples/internal/demo"
	"github.com/jryannel/ragit/extract"
	"github.com/jryannel/ragit/jobs"
)

// Its own tenant, so it never fights extract-only over the same corpus.
var tenant = uuid.MustParse("c0000000-0000-4000-8000-000000000003")

// queue is the one ragit's job args name. A host application's River client has
// to configure it or the workers register and then never run — the jobs are
// inserted, sit in a queue nobody polls, and nothing reports a problem.
const queue = "ragit_process_document"

func main() {
	extract.RunIsolatedChildIfInvoked()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nasync:", err)
		os.Exit(1)
	}
}

func run() error {
	sweepEvery := flag.Duration("sweep", 5*time.Second, "retention sweep interval")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return err
	}
	if err := cfg.RequireEdenAI(); err != nil {
		return err
	}

	env, err := bootstrap.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	defer env.Close()

	// River owns its own migration line, exactly as ragit does — a third one in
	// this database, alongside ragit_migrations and the host application's.
	if err := migrateRiver(ctx, env); err != nil {
		return err
	}

	client, err := embed.NewOpenAICompatible(embed.OpenAICompatibleConfig{
		APIKey:    cfg.EdenAIKey,
		BaseURL:   cfg.EdenAIBaseURL,
		Model:     cfg.EdenAIModel,
		Dimension: cfg.EmbeddingDim,
	})
	if err != nil {
		return err
	}
	embedder := demo.NewCounting(client)

	processor := ragit.New(
		env.App,
		extract.NewChain(
			extract.NewXbergExtractor(cfg.XbergURL, 0),
			extract.NewIsolatedExtractor(),
			extract.NewLocalExtractor(),
		),
		chunk.New(chunk.DefaultConfig()),
		embedder,
		env.Store,
	)

	// ragit does not own a river.Client. The host application builds one and
	// registers ragit's workers into it, so they share a queue and a shutdown
	// with the application's own jobs rather than running a second worker pool.
	workers := river.NewWorkers()
	river.AddWorker(workers, jobs.NewProcessDocumentWorker(processor))
	river.AddWorker(workers, jobs.NewDeleteDocumentWorker(processor))
	river.AddWorker(workers, jobs.NewDeleteExpiredWorker(processor))

	riverClient, err := river.NewClient(riverpgxv5.New(env.App), &river.Config{
		Queues:  map[string]river.QueueConfig{queue: {MaxWorkers: 2}},
		Workers: workers,
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(*sweepEvery),
				func() (river.JobArgs, *river.InsertOpts) { return jobs.DeleteExpiredArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		return fmt.Errorf("build river client: %w", err)
	}

	// Subscribe before Start, or the first jobs finish before anyone is
	// listening and the wait below hangs on events that already happened.
	events, unsubscribe := riverClient.Subscribe(
		river.EventKindJobCompleted,
		river.EventKindJobFailed,
		river.EventKindJobCancelled,
	)
	defer unsubscribe()

	if err := riverClient.Start(ctx); err != nil {
		return fmt.Errorf("start river: %w", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		_ = riverClient.Stop(stopCtx)
	}()

	fmt.Printf("queue             %s\n", queue)
	fmt.Printf("embedding space   %s\n", embed.Fingerprint(embedder))
	fmt.Printf("workers           process_document, delete_document, delete_expired\n")

	// ── Enqueue ───────────────────────────────────────────────────────────
	demo.Section("Enqueue: nothing is processed inline")

	docs, err := fixtures.All()
	if err != nil {
		return err
	}

	queued := 0
	for _, doc := range docs {
		id, created, err := demo.EnsureDocument(ctx, env.App, processor, tenant, doc)
		if err != nil {
			return err
		}
		if !created {
			// Already ingested by a previous run. Enqueuing anyway would be
			// harmless — ProcessDocument resumes — but it would also make the
			// job counts below meaningless.
			fmt.Printf("  %-18s already indexed, not re-enqueued\n", doc.Filename)
			continue
		}
		if _, err := riverClient.Insert(ctx, jobs.ProcessDocumentArgs{
			DocumentID: id,
			TenantID:   tenant,
		}, nil); err != nil {
			return fmt.Errorf("enqueue %s: %w", doc.Filename, err)
		}
		queued++
		fmt.Printf("  %-18s enqueued as %s\n", doc.Filename, jobs.ProcessDocumentArgs{}.Kind())
	}

	if queued > 0 {
		demo.Section("Wait for the workers")
		if err := waitFor(ctx, events, queued, 5*time.Minute); err != nil {
			return err
		}
	}

	demo.Section("Catalog")
	listed, err := processor.ListDocuments(ctx, ragit.Tenant(tenant), ragit.ListFilter{})
	if err != nil {
		return err
	}
	demo.PrintDocuments(listed)
	fmt.Println("\n  Nothing above called ProcessDocument. Every row reached `ready` through a")
	fmt.Println("  River job, which is the arrangement §7 argues for: one resumable job per")
	fmt.Println("  document rather than a chain of per-stage jobs.")

	// ── Retention ─────────────────────────────────────────────────────────
	//
	// The sweep is the one worker that finds its own work, and the only path
	// in ragit that reads across tenants — so it is also the only user of
	// WithMaintenance. Worth exercising precisely because nothing else does.
	demo.Section("Retention: a document that expires before the next sweep")

	expired := time.Now().Add(-time.Hour)
	ephemeralID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID:  tenant,
		Filename:  "ephemeral.md",
		MimeType:  "text/markdown",
		Data:      []byte("# Ephemeral\n\nAn attachment with a retention clock that has already run out.\n"),
		ExpiresAt: &expired,
	})
	if err != nil {
		return err
	}
	fmt.Printf("  created %s with expires_at %s (in the past)\n",
		"ephemeral.md", expired.Format(time.RFC3339))

	if err := waitGone(ctx, processor, tenant, ephemeralID, 90*time.Second); err != nil {
		return err
	}
	fmt.Printf("  the periodic sweep removed it, across tenants, under WithMaintenance\n")

	// ── Deletion through the queue ────────────────────────────────────────
	demo.Section("Delete: the other worker")

	// A throwaway rather than one of the fixtures. Deleting real corpus here
	// would make every re-run re-ingest and re-embed it, which is the opposite
	// of what these examples are supposed to demonstrate.
	doomedID, err := processor.CreateDocument(ctx, ragit.DocumentInput{
		TenantID: tenant,
		Filename: "doomed.md",
		MimeType: "text/markdown",
		Data:     []byte("# Doomed\n\nCreated only to be deleted through the queue.\n"),
	})
	if err != nil {
		return err
	}
	if _, err := riverClient.Insert(ctx, jobs.DeleteDocumentArgs{
		DocumentID: doomedID,
		TenantID:   tenant,
	}, nil); err != nil {
		return err
	}
	fmt.Printf("  enqueued deletion of doomed.md\n")
	if err := waitGone(ctx, processor, tenant, doomedID, 60*time.Second); err != nil {
		return err
	}
	fmt.Printf("  gone — row, chunks (cascaded) and stored bytes\n")

	demo.Section("Cost")
	fmt.Printf("  %d embedding call(s), %d text(s) embedded.\n", embedder.Calls(), embedder.Texts())
	fmt.Println("  Re-running enqueues nothing: the documents are already indexed, and the")
	fmt.Println("  application's own table is what knows that.")
	return nil
}

// migrateRiver applies River's schema and then re-grants.
//
// The re-grant is the part worth noticing. bootstrap.Setup already granted the
// application role SELECT/INSERT/UPDATE/DELETE on every table — but GRANT ...
// ON ALL TABLES expands at the moment it runs, and River's tables do not exist
// until now. Without the second grant the role can insert documents perfectly
// well and then fail on the first job insert with a bare permission denied.
func migrateRiver(ctx context.Context, env *bootstrap.Env) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(env.Admin), nil)
	if err != nil {
		return fmt.Errorf("build river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("apply river migrations: %w", err)
	}
	return bootstrap.GrantAppRole(ctx, env.Admin, env.Cfg)
}

// waitFor collects n terminal job events, reporting each as it lands.
func waitFor(ctx context.Context, events <-chan *river.Event, n int, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	seen := 0
	for seen < n {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timed out after %s with %d of %d jobs finished", timeout, seen, n)
		case ev := <-events:
			// The periodic retention sweep also lands here. It is not one of
			// the jobs being waited on, so it is reported and not counted.
			if ev.Job.Kind == (jobs.DeleteExpiredArgs{}).Kind() {
				fmt.Printf("  (retention sweep ran: %s)\n", ev.Job.State)
				continue
			}
			seen++
			switch ev.Kind {
			case river.EventKindJobCompleted:
				fmt.Printf("  %-24s waited %s, worked %s\n", ev.Job.Kind,
					queued(ev.Job), worked(ev.Job))
			default:
				// A cancelled job is §7's "verdict on the document": the
				// worker classified the error as not worth retrying.
				fmt.Printf("  %-24s %s: %v\n", ev.Job.Kind, ev.Job.State, ev.Job.Errors)
			}
		}
	}
	return nil
}

// queued is how long the job sat in the queue, and worked is how long the
// worker actually took. Reporting only one of them is how a queue looks fast
// while doing nothing: AttemptedAt-CreatedAt is latency, not work.
func queued(job *rivertype.JobRow) time.Duration {
	if job.AttemptedAt == nil {
		return 0
	}
	return job.AttemptedAt.Sub(job.CreatedAt).Round(time.Millisecond)
}

func worked(job *rivertype.JobRow) time.Duration {
	if job.AttemptedAt == nil || job.FinalizedAt == nil {
		return 0
	}
	return job.FinalizedAt.Sub(*job.AttemptedAt).Round(time.Millisecond)
}

// waitGone polls until a document is no longer visible to its tenant.
func waitGone(ctx context.Context, p *ragit.Processor, tenantID, id uuid.UUID, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := p.GetDocument(ctx, ragit.Tenant(tenantID), id)
		if errors.Is(err, ragit.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("document %s still present after %s", id, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
