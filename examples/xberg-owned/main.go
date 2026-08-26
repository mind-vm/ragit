// Command xberg-owned gives xberg the whole front half of the pipeline.
//
// One /extract call returns the document extracted, chunked, and embedded —
// 768-dimension vectors from xberg's local ONNX preset, no embedding provider
// and no API key anywhere. ragit is left with storage and retrieval.
//
// This example is why ragit.IngestPrepared exists. It was written first as a
// bypass — reaching past the Processor to write ragit.Chunk rows with sqlb —
// and the five things that cost is the finding it produced; see ingest.go and
// docs/examples-plan.md. It now uses the seam that finding argued for, and the
// diff between the two is the point.
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
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/chunk"
	"github.com/mind-vm/ragit/examples/fixtures"
	"github.com/mind-vm/ragit/examples/internal/bootstrap"
	"github.com/mind-vm/ragit/examples/internal/demo"
	"github.com/mind-vm/ragit/examples/xberg-owned/migrations"
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

	// No extractor and no chunker: this program never calls either, and
	// ragit.New now says so rather than leaving it a memorised fact about
	// which methods avoid a nil field. The embedder is here for queries only —
	// the corpus is embedded by xberg, inside the extract call.
	processor := ragit.New(env.App, nil, nil, embedder, env.Store)

	fmt.Printf("database          %s\n", database)
	fmt.Printf("embedding space   %s\n", EmbeddingSpace.Fingerprint())
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

	var ids []uuid.UUID
	for _, doc := range docs {
		id, created, err := demo.EnsureDocument(ctx, env.App, processor, tenantA, doc)
		if err != nil {
			return err
		}
		ids = append(ids, id)
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
		if err := storePrepared(ctx, processor, tenantA, id, result); err != nil {
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
	fmt.Println("\n  Every column here used to be written by hand in ingest.go, with nothing")
	fmt.Println("  checking the set was complete. IngestPrepared writes the same terminal")
	fmt.Println("  state ProcessDocument does, so the catalog cannot disagree with the chunks.")

	// ── The resume guard, and its edge ────────────────────────────────────
	demo.Section("Reprocessing: what the resume guard covers, and what it cannot")

	beforeCalls, beforeChunks := client.ExtractCalls(), client.ChunksEmbedded()
	beforeIDs, err := chunkRowIDs(ctx, processor, scopeA, ids)
	if err != nil {
		return err
	}

	for i, doc := range docs {
		result, err := client.ExtractChunkEmbed(ctx, doc.Data, doc.Filename)
		if err != nil {
			return err
		}
		if err := storePrepared(ctx, processor, tenantA, ids[i], result); err != nil {
			return err
		}
	}

	afterIDs, err := chunkRowIDs(ctx, processor, scopeA, ids)
	if err != nil {
		return err
	}
	fmt.Printf("  ragit   %d of %d chunk row(s) rewritten\n", rewrittenRows(beforeIDs, afterIDs), len(beforeIDs))
	fmt.Printf("  xberg   %d extract call(s), %d chunk(s) re-embedded\n",
		client.ExtractCalls()-beforeCalls, client.ChunksEmbedded()-beforeChunks)
	fmt.Println("\n  IngestPrepared runs the same guard ProcessDocument does — each chunk's")
	fmt.Println("  content and fingerprint against what is stored — so an unchanged corpus is")
	fmt.Println("  left alone: no delete, no rewrite, and nothing re-billed had these vectors")
	fmt.Println("  come from a hosted model. Before the seam existed this path deleted and")
	fmt.Println("  rewrote every chunk on every pass.")
	fmt.Println("  What no guard can do is unspend the extract call above. Here embedding")
	fmt.Println("  rides along with extraction in one request, so by the time ragit sees the")
	fmt.Println("  chunks the work is already paid for. extract-only re-extracts on a second")
	fmt.Println("  pass too; what it reports as zero is embedding calls, because there")
	fmt.Println("  embedding is ragit's own step and sits behind the guard. Skipping the")
	fmt.Println("  upstream call is a decision this program can make and the library cannot:")
	fmt.Println("  whether the bytes changed.")

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
	fmt.Println("  Both depend on the document's attributes reaching every chunk. That used to")
	fmt.Println("  be a hand-written copy nobody would have complained about forgetting; it is")
	fmt.Println("  now IngestPrepared's job, along with the scope and expiry columns.")

	other, err := processor.VectorSearch(ctx, ragit.Tenant(tenantB), *query, opts)
	if err != nil {
		return err
	}
	if len(other) != 0 {
		return fmt.Errorf("tenant B saw %d results it should not have", len(other))
	}
	fmt.Printf("  tenant B: %d results. This held even when the example wrote chunk rows by\n", len(other))
	fmt.Println("  hand, because confinement is the database's and not the Processor's — the")
	fmt.Println("  argument for pushing a property down into the schema rather than into Go.")

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

// chunkRowIDs snapshots the identity of every stored chunk, so "nothing was
// rewritten" can be told from "deleted and rewritten with the same text".
func chunkRowIDs(ctx context.Context, processor *ragit.Processor, scope ragit.Scope, ids []uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for _, id := range ids {
		chunks, err := processor.ListChunks(ctx, scope, id)
		if err != nil {
			return nil, fmt.Errorf("list chunks of %s: %w", id, err)
		}
		for _, c := range chunks {
			out = append(out, c.ID)
		}
	}
	return out, nil
}

// rewrittenRows counts chunks that are not the rows that were there before.
func rewrittenRows(before, after []uuid.UUID) int {
	kept := make(map[uuid.UUID]bool, len(before))
	for _, id := range before {
		kept[id] = true
	}
	n := 0
	for _, id := range after {
		if !kept[id] {
			n++
		}
	}
	return n
}
