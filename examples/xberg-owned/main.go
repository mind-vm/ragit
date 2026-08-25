// Command xberg-owned gives xberg the whole front half of the pipeline.
//
// One /extract call returns the document extracted, chunked, and embedded —
// 768-dimension vectors from xberg's local ONNX preset, no embedding provider
// and no API key anywhere. ragit is left with storage and retrieval.
//
// It does not fit ragit's API, and that is what it is for. Processor runs
// extract → chunk → embed as one sealed sequence, so this reaches past it and
// writes ragit.Chunk rows with sqlb — the escape hatch the README advertises for
// reads, used here for writes. Every place that hurts is marked FRICTION in
// ingest.go, and summarised in docs/examples-plan.md.
//
//	cd examples && make up && make verify
//	go run ./xberg-owned
//
// No EDENAI_API_KEY: nothing in this pipeline talks to a hosted provider. The
// first run downloads the ONNX model into the xberg container's cache and is
// slow.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jryannel/sqlb"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/chunk"
	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/examples/fixtures"
	"github.com/jryannel/ragit/examples/internal/bootstrap"
	"github.com/jryannel/ragit/examples/internal/demo"
	"github.com/jryannel/ragit/examples/xberg-owned/migrations"
)

// A different database from the extract-only example, because a different
// embedding width means a different column type and there is no migration from
// one to the other that keeps the rows.
const database = "ragit_examples_768"

var (
	tenantA = uuid.MustParse("a0000000-0000-4000-8000-000000000001")
	tenantB = uuid.MustParse("b0000000-0000-4000-8000-000000000002")
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nxberg-owned:", err)
		os.Exit(1)
	}
}

func run() error {
	query := flag.String("query", "how do I reset my password?", "vector-search query")
	textQuery := flag.String("text-query", "refund supervisor approval", "full-text query")
	topK := flag.Int("topk", 5, "maximum results per search")
	minScore := flag.Float64("min-score", 0, "cosine-similarity cutoff for vector search")
	reset := flag.Bool("reset", false, "delete this example's documents first")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	base, err := bootstrap.LoadConfig()
	if err != nil {
		return err
	}
	cfg, err := base.WithDatabase(database)
	if err != nil {
		return err
	}

	env, err := bootstrap.SetupWith(ctx, cfg, bootstrap.Options{
		CreateDatabase: true,
		// ragit.Migrate would apply the library's embedded 1536-dimension
		// schema. See migrations/migrations.go for why this exists.
		MigrateRagit: migrations.Up,
	})
	if err != nil {
		return err
	}
	defer env.Close()

	client := NewClient(base.XbergURL, chunk.DefaultConfig().Size, chunk.DefaultConfig().Overlap)
	embedder := newXbergEmbedder(client)

	// FRICTION 0, before a single document is touched: ragit.New demands an
	// Extractor and a *chunk.Chunker, and this program will never call either.
	// It needs a Processor for CreateDocument, GetDocument, ListDocuments,
	// VectorSearch, FullTextSearch and DeleteDocument — the whole catalog and
	// retrieval surface hangs off the same struct as the ingestion pipeline —
	// so it constructs one with two nil dependencies and relies on knowing
	// which methods stay away from them. That is not a contract, it is a
	// memorised fact about the implementation.
	processor := ragit.New(env.App, nil, nil, embedder, env.Store)

	fmt.Printf("database          %s\n", database)
	fmt.Printf("embedding space   %s\n", embed.Fingerprint(embedder))
	fmt.Printf("chunker           xberg markdown, max_chars=%d overlap=%d\n", client.MaxChars, client.Overlap)
	fmt.Printf("provider calls    none — the ONNX model runs inside the xberg container\n")

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
	demo.Section("Ingest: one call extracts, chunks and embeds")

	for _, doc := range docs {
		id, created, err := demo.EnsureDocument(ctx, env.App, processor, tenantA, doc)
		if err != nil {
			return err
		}
		state := "reused"
		if created {
			state = "new"
		}

		start := time.Now()
		result, err := client.ExtractChunkEmbed(ctx, doc.Data, doc.Filename)
		if err != nil {
			fmt.Printf("  %-18s %-8s  extract FAILED: %v\n", doc.Filename, state, err)
			continue
		}
		if err := storePrepared(ctx, env.App, processor, tenantA, id, result, embedder); err != nil {
			fmt.Printf("  %-18s %-8s  store FAILED: %v\n", doc.Filename, state, err)
			continue
		}
		fmt.Printf("  %-18s %-8s %8s  %d chunks, %d embedded by xberg\n",
			doc.Filename, state, time.Since(start).Round(time.Millisecond),
			len(result.Chunks), len(result.Chunks))
	}

	// ── Catalog ───────────────────────────────────────────────────────────
	demo.Section("Catalog")

	scopeA := ragit.Tenant(tenantA)
	listed, err := processor.ListDocuments(ctx, scopeA, ragit.ListFilter{})
	if err != nil {
		return err
	}
	demo.PrintDocuments(listed)
	fmt.Println("\n  Every column here was written by hand in ingest.go. Processor.finish is")
	fmt.Println("  unexported, so nothing checks the set is complete — a missing one shows up")
	fmt.Println("  as a catalog that quietly disagrees with the chunks table.")

	// ── The resume guard, absent ──────────────────────────────────────────
	demo.Section("Reprocessing: no resume guard on this path")

	beforeCalls, beforeChunks := client.ExtractCalls(), client.ChunksEmbedded()
	for _, doc := range docs {
		id, _, err := demo.EnsureDocument(ctx, env.App, processor, tenantA, doc)
		if err != nil {
			return err
		}
		result, err := client.ExtractChunkEmbed(ctx, doc.Data, doc.Filename)
		if err != nil {
			return err
		}
		if err := storePrepared(ctx, env.App, processor, tenantA, id, result, embedder); err != nil {
			return err
		}
	}
	fmt.Printf("  second pass: %d extract call(s), %d chunk(s) re-embedded\n",
		client.ExtractCalls()-beforeCalls, client.ChunksEmbedded()-beforeChunks)
	fmt.Println("  extract-only shows 0 here. ragit's resume check lives inside")
	fmt.Println("  embedAndStore, which this path never reaches, so every chunk is embedded")
	fmt.Println("  again. Local ONNX makes that cost CPU rather than money — but the guard is")
	fmt.Println("  gone, not merely cheaper, and a hosted embedding model would be re-billed.")

	// ── Retrieval ─────────────────────────────────────────────────────────
	demo.Section(fmt.Sprintf("Vector search: %q", *query))

	opts := ragit.SearchOptions{TopK: *topK, MinScore: *minScore}
	results, err := processor.VectorSearch(ctx, scopeA, *query, opts)
	if err != nil {
		return err
	}
	demo.PrintResults(results)
	fmt.Println("\n  The query was embedded by posting it to /extract as a one-chunk")
	fmt.Println("  pseudo-document: xberg has no HTTP endpoint for embedding text. A full")
	fmt.Println("  extraction round trip — MIME detection, format dispatch, chunking — to turn")
	fmt.Println("  six words into a vector.")

	demo.Section(fmt.Sprintf("Full-text search: %q", *textQuery))

	textResults, err := processor.FullTextSearch(ctx, scopeA, *textQuery, opts)
	if err != nil {
		return err
	}
	demo.PrintResults(textResults)

	// ── Narrowing and confinement ─────────────────────────────────────────
	demo.Section("Attributes narrow; Scope confines")

	narrowed, err := processor.VectorSearch(ctx, scopeA, *query, ragit.SearchOptions{
		TopK: *topK, MinScore: *minScore,
		Attributes: ragit.Attributes{"team": "warehouse"},
	})
	if err != nil {
		return err
	}
	fmt.Printf("  unfiltered: %d results; team=warehouse: %d results\n", len(results), len(narrowed))
	fmt.Println("  Both depend on ingest.go having copied the document's attributes onto every")
	fmt.Println("  chunk by hand. Nothing would have complained if it had not.")

	other, err := processor.VectorSearch(ctx, ragit.Tenant(tenantB), *query, opts)
	if err != nil {
		return err
	}
	if len(other) != 0 {
		return fmt.Errorf("tenant B saw %d results it should not have", len(other))
	}
	fmt.Printf("  tenant B: %d results — confinement survives the bypass, because it is the\n", len(other))
	fmt.Println("  database's and not the Processor's.")

	// ── Cost ──────────────────────────────────────────────────────────────
	demo.Section("Cost")
	fmt.Printf("  %d /extract call(s), %d chunk(s) embedded, %d of those calls were queries.\n",
		client.ExtractCalls(), client.ChunksEmbedded(), client.QueryEmbeds())
	fmt.Println("  Nothing was billed. The counters are on the HTTP client rather than on the")
	fmt.Println("  Embedder, because on this path the corpus never goes through an Embedder at")
	fmt.Println("  all — an embed.Embedder decorator would have reported almost none of this.")

	return nil
}

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
