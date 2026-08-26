// Command extract-only is the pipeline ragit was designed for.
//
// xberg extracts. Everything after that is ragit's: a Markdown-aware chunker in
// Go, embeddings through one OpenAI-wire-compatible client pointed at EdenAI,
// storage and retrieval in Postgres/pgvector. That split is docs/design.md §5's
// conclusion, and this program is the test of whether the library actually lets
// a consumer wire it up from the outside.
//
// It is a documentation test as much as a demo: every line of setup here should
// be something the README told you to do.
//
//	cd examples && make up && make verify
//	set -a; . ~/.config/envs/valiro-go.env; set +a
//	go run ./extract-only
//
// Re-running is cheap on purpose. The corpus belongs to a fixed tenant, uploads
// are deduplicated against the application's own table, and ProcessDocument
// resumes — so a second run should embed nothing and the program says so.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/chunk"
	"github.com/mind-vm/ragit/embed"
	"github.com/mind-vm/ragit/examples/fixtures"
	"github.com/mind-vm/ragit/examples/internal/bootstrap"
	"github.com/mind-vm/ragit/examples/internal/demo"
	"github.com/mind-vm/ragit/extract"
)

// Fixed rather than random, so re-running reuses the corpus instead of
// re-embedding it. TenantB never has anything ingested into it — it exists
// only to be searched from, which is the confinement check.
var (
	tenantA = uuid.MustParse("a0000000-0000-4000-8000-000000000001")
	tenantB = uuid.MustParse("b0000000-0000-4000-8000-000000000002")
)

func main() {
	// The first statement in main(), before flags, before logging, before
	// anything.
	//
	// IsolatedExtractor contains a parse by re-invoking this binary as a
	// short-lived child with a memory cap. A library cannot arrange that for
	// itself — ragit does not own main() — so the host application has to hand
	// it the entry point. Omit this and the children run this program's normal
	// startup instead of parsing, IsolatedExtractor reports itself
	// unavailable, and the chain quietly degrades to direct local parsing.
	// Degraded, not broken, and nothing says so at runtime.
	extract.RunIsolatedChildIfInvoked()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nextract-only:", err)
		os.Exit(1)
	}
}

func run() error {
	query := flag.String("query", "how do I reset my password?", "vector-search query")
	textQuery := flag.String("text-query", "refund supervisor approval",
		"full-text query; keywords, not a question — see the note this program prints")
	topK := flag.Int("topk", 5, "maximum results per search")
	minScore := flag.Float64("min-score", 0,
		"cosine-similarity cutoff for vector search; 0 shows everything so the band can be calibrated")
	reset := flag.Bool("reset", false, "delete this example's documents first, forcing a full re-embed")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
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

	// ── Wiring ────────────────────────────────────────────────────────────
	//
	// Three extractors in preference order. The fallback fires only when an
	// extractor was *unavailable* — connection refused, a 5xx, a child that
	// could not start. A document xberg parsed and rejected stops the chain,
	// because retrying a rejected document down the chain means feeding bytes
	// that already broke one parser into progressively less-contained code.
	extractor := extract.NewChain(
		extract.NewXbergExtractor(cfg.XbergURL, 0),
		extract.NewIsolatedExtractor(),
		extract.NewLocalExtractor(),
	)

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
		extractor,
		chunk.New(chunk.DefaultConfig()),
		embedder,
		env.Store,
	)

	fmt.Printf("extractor chain   %d layers (xberg → isolated child → local parsers)\n", extractor.Len())
	fmt.Printf("embedding space   %s\n", embed.Fingerprint(embedder))
	fmt.Printf("chunker           %+v\n", chunk.DefaultConfig())

	if *reset {
		if err := resetCorpus(ctx, env, processor); err != nil {
			return err
		}
	}

	docs, err := fixtures.All()
	if err != nil {
		return err
	}

	// ── Ingestion ─────────────────────────────────────────────────────────
	demo.Section("Ingest")

	ids := make([]uuid.UUID, 0, len(docs))
	for _, doc := range docs {
		id, created, err := demo.EnsureDocument(ctx, env.App, processor, tenantA, doc)
		if err != nil {
			return err
		}
		ids = append(ids, id)

		before := embedder.Calls()
		start := time.Now()
		// CreateDocument and ProcessDocument rather than Ingest: this is the
		// split a real deployment runs, with a River job in between. Calling
		// them back to back keeps the example one process without pretending
		// the seam is not there.
		err = processor.ProcessDocument(ctx, id, tenantA)
		elapsed := time.Since(start).Round(time.Millisecond)

		state := "reused"
		if created {
			state = "new"
		}
		if err != nil {
			// Reported, not fatal. The document still reaches a terminal
			// state, and the catalog below is where a host application would
			// look to find out why.
			fmt.Printf("  %-18s %-8s %8s  FAILED: %v\n", doc.Filename, state, elapsed, err)
			continue
		}
		fmt.Printf("  %-18s %-8s %8s  %d embedding call(s)\n",
			doc.Filename, state, elapsed, embedder.Calls()-before)
	}

	// ── Catalog ───────────────────────────────────────────────────────────
	demo.Section("Catalog")

	scopeA := ragit.Tenant(tenantA)
	listed, err := processor.ListDocuments(ctx, scopeA, ragit.ListFilter{})
	if err != nil {
		return err
	}
	demo.PrintDocuments(listed)

	// ── The resume guard ──────────────────────────────────────────────────
	//
	// The claim worth testing is a claim about not doing work. Running the
	// whole pipeline again over unchanged documents must re-extract and
	// re-chunk — those are cheap and local — but must not re-embed, because
	// that is the part with an invoice attached.
	demo.Section("Resume: reprocessing unchanged documents")

	beforeResume := embedder.Calls()
	for _, id := range ids {
		if err := processor.ProcessDocument(ctx, id, tenantA); err != nil {
			fmt.Printf("  reprocess %s: %v\n", id, err)
		}
	}
	if calls := embedder.Calls() - beforeResume; calls == 0 {
		fmt.Printf("  %d embedding calls on the second pass — the resume check held\n", calls)
	} else {
		fmt.Printf("  %d embedding calls on the second pass — expected 0; the corpus was re-billed\n", calls)
	}

	// ── Retrieval ─────────────────────────────────────────────────────────
	//
	// Two separate queries, not one fused hybrid endpoint. The scores are not
	// comparable across them: one is cosine similarity, the other a ts_rank.
	demo.Section(fmt.Sprintf("Vector search: %q", *query))

	opts := ragit.SearchOptions{TopK: *topK, MinScore: *minScore}
	results, err := processor.VectorSearch(ctx, scopeA, *query, opts)
	if err != nil {
		return err
	}
	demo.PrintResults(results)
	fmt.Printf("\n  MinScore is %.2f. ragit ships no default above zero on purpose — the band\n"+
		"  separating a relevant match from noise belongs to the embedding model, not to\n"+
		"  retrieval. Calibrate it against the scores above.\n", *minScore)

	demo.Section(fmt.Sprintf("Full-text search: %q", *textQuery))

	textResults, err := processor.FullTextSearch(ctx, scopeA, *textQuery, opts)
	if err != nil {
		return err
	}
	demo.PrintResults(textResults)

	// The query above is keywords, and the vector query above it is a
	// question. That is not stylistic.
	//
	// ragit's full-text side is websearch_to_tsquery over a 'simple'-config
	// tsvector. 'simple' has no stopword dictionary and no stemmer, so every
	// token survives — and websearch_to_tsquery ANDs them. Asking "how do I
	// reset my password?" therefore demands a chunk containing "how" AND "do"
	// AND "i" AND "my", which no chunk has. It returns nothing, silently, and
	// looks like an empty corpus.
	sameQuestion, err := processor.FullTextSearch(ctx, scopeA, *query, opts)
	if err != nil {
		return err
	}
	fmt.Printf("\n  The vector query as full text (%q): %d results.\n", *query, len(sameQuestion))
	fmt.Println("  'simple' keeps stopwords and websearch_to_tsquery ANDs them, so a")
	fmt.Println("  natural-language question asks for chunks containing \"how\" and \"do\" and")
	fmt.Println("  \"i\". Route questions to vector search and keywords to this one.")

	// ── Narrowing vs. confining ───────────────────────────────────────────
	demo.Section("Attributes narrow the same search")

	narrowed, err := processor.VectorSearch(ctx, scopeA, *query, ragit.SearchOptions{
		TopK:       *topK,
		MinScore:   *minScore,
		Attributes: ragit.Attributes{"team": "warehouse"},
	})
	if err != nil {
		return err
	}
	fmt.Printf("  unfiltered: %d results; team=warehouse: %d results\n\n", len(results), len(narrowed))
	demo.PrintResults(narrowed)
	fmt.Println("\n  An empty attribute filter matches everything — the opposite of Scope's rule,")
	fmt.Println("  and deliberate: a forgotten filter should return more rows, not none. Which is")
	fmt.Println("  exactly why attributes must never be used for access control.")

	// ── Confinement ───────────────────────────────────────────────────────
	demo.Section("The same search, as a tenant that owns nothing")

	other, err := processor.VectorSearch(ctx, ragit.Tenant(tenantB), *query, opts)
	if err != nil {
		return err
	}
	demo.PrintResults(other)
	if len(other) == 0 {
		fmt.Println("\n  Confined twice over: the query carries the tenant predicate, and beneath it")
		fmt.Println("  FORCE ROW LEVEL SECURITY would refuse the rows to a query that forgot.")
	} else {
		return fmt.Errorf("tenant B saw %d results it should not have", len(other))
	}

	// ── Cost ──────────────────────────────────────────────────────────────
	demo.Section("Cost")
	fmt.Printf("  %d embedding call(s) this run, %d text(s) embedded.\n",
		embedder.Calls(), embedder.Texts())
	fmt.Println("  Every one is billed. Re-running without -reset should show only the")
	fmt.Println("  query embeddings — the corpus is already in this embedding space.")

	return nil
}

// resetCorpus deletes the example's documents and the application's record of
// them, so the next ingest is a real one.
func resetCorpus(ctx context.Context, env *bootstrap.Env, processor *ragit.Processor) error {
	docs, err := processor.ListDocuments(ctx, ragit.Tenant(tenantA), ragit.ListFilter{Limit: 500})
	if err != nil {
		return err
	}
	for _, d := range docs {
		if err := processor.DeleteDocument(ctx, tenantA, d.ID); err != nil {
			return fmt.Errorf("delete %s: %w", d.Filename, err)
		}
	}
	if _, err := sqlb.DeleteRows[bootstrap.Upload]().
		Where(sqlb.RawPred("tenant_id = ?", tenantA)).
		Exec(ctx, env.App); err != nil {
		return fmt.Errorf("clear demo_uploads: %w", err)
	}
	fmt.Printf("reset             deleted %d document(s)\n", len(docs))
	return nil
}
