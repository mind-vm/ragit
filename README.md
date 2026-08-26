# ragit

A reusable RAG pipeline for Go: extract a document, chunk it, embed it, store
it in Postgres/pgvector, and retrieve it — as a library a SaaS application
imports rather than a service it runs.

[`docs/design.md`](docs/design.md) is the real documentation: the full design,
and the production incidents that shaped it. This file is the entry point.

## What it does

```
Upload → object storage → River job → extract → chunk → embed → pgvector → retrieval
```

- **Extraction** through an [xberg](https://github.com/xberg-io/xberg) sidecar
  when one is configured, falling back to a memory-capped child process and
  then to in-process parsers. The fallback fires only when an extractor was
  *unavailable*, never when it rejected the document.
- **Chunking** in Go, Markdown-heading aware, because chunk metadata has to
  round-trip into a citation.
- **Embedding** through a single OpenAI-wire-compatible client, with a
  `provider|model|dimension` fingerprint per chunk.
- **Retrieval** as two separate calls — vector and full-text — rather than one
  fused endpoint.
- **One resumable River job** per document, checkpointed per embedding batch,
  so a retry resumes instead of re-billing the embedding provider.
- **Or none of the front half**: a service that extracts, chunks and embeds in
  one call hands the result to `IngestPrepared` and keeps everything above.

## Wiring it up

```go
pool, err := ragit.NewPool(ctx, os.Getenv("DATABASE_URL"))
if err != nil { return err }

if err := ragit.Migrate(ctx, pool); err != nil { return err }

processor := ragit.New(pool,
    extract.NewChain(
        extract.NewXbergExtractor(os.Getenv("XBERG_URL"), 0), // optional
        extract.NewIsolatedExtractor(),
        extract.NewLocalExtractor(),
    ),
    chunk.New(chunk.DefaultConfig()),
    embedder,
    objectStore,
)
```

Four things are easy to get wrong, and three of them fail quietly:

**Use `ragit.NewPool`, or register the codec yourself.** pgvector's binary
codec needs the extension's OID, which only exists once the extension is
installed, so it is registered per connection: `cfg.AfterConnect =
sqlb.RegisterVectorType`. Without it embeddings still move — as text, several
times slower.

**Do not connect as a superuser.** ragit's tables carry `FORCE ROW LEVEL
SECURITY`, and PostgreSQL exempts superusers and `BYPASSRLS` roles from row-level
security regardless. The stock `postgres` image's `POSTGRES_USER` is a
superuser, so an application connecting as one has these policies silently
doing nothing and is relying on the query predicates alone.

**Call `extract.RunIsolatedChildIfInvoked()` first in `main()`** if you use
`IsolatedExtractor`. A library cannot re-invoke itself the way an application
can — ragit does not own `main()`. Without it the isolation layer reports
itself unavailable and the chain degrades to direct local parsing.

```go
func main() {
    extract.RunIsolatedChildIfInvoked()
    // ... normal startup
}
```

**Calibrate `MinScore` per embedding model.** The band separating a relevant
match from noise is a property of the model, not of retrieval. ragit ships no
default beyond zero on purpose.

## Retrieval is confined by construction

Every read takes a `Scope`, and its zero value matches no rows:

```go
results, err := processor.VectorSearch(ctx,
    ragit.Tenant(tenantID).A(companyID).B(coachID),
    "how do I reset my password?",
    ragit.SearchOptions{TopK: 8, MinScore: 0.6},
)
```

A dimension nobody mentions matches only rows where it is NULL, so a corpus
that never sets the scope columns works unchanged while one that does cannot
leak across a boundary because a caller left a field out. Unbounded access is
`AnyA()` / `AnyB()` — a separate predicate, never a magic value in the column.

## Filtering by your own facts

Documents carry application-supplied key/value `Attributes`, stored separately
from the extractor's `Metadata` so a new xberg field can never collide with one
of your keys:

```go
processor.Ingest(ctx, ragit.DocumentInput{
    TenantID:   tenantID,
    Attributes: ragit.Attributes{"course": courseID, "kind": "recording"},
    ...
})

results, err := processor.VectorSearch(ctx, scope, query, ragit.SearchOptions{
    TopK:       8,
    Attributes: ragit.Attributes{"course": courseID},
})
```

Matching is JSONB containment, so a filter names only the pairs it cares about,
and multiple pairs are ANDed. Attributes are denormalized onto chunks and GIN
indexed, so the filter rides alongside the vector scan rather than fighting it —
which is also why changing them goes through `SetDocumentAttributes`, which
re-stamps the chunks. Reprocessing will not fix a stale copy: the resume check
sees identical content and skips the rewrite.

**Attributes narrow; they do not confine.** An empty filter matches everything,
which is the opposite of `Scope`'"'"'s rule and deliberate — a forgotten filter
should return more rows, not none. So do not use them for access control: a
caller that must not see a document should be outside its scope, not merely
failing to match a label.

## Schema

The schema is declared in [`ragitschema`](ragitschema/schema.go) and everything
else is generated from it:

```
go run ./cmd/ragit-gen          # migrations/ and the *_gen.go models
```

Tables are prefixed `ragit_` and tracked in ragit's own `ragit_migrations`
version table, so they sit in a host application's database without colliding
with its schema or its migration sequence. The embedding dimension is an
argument to the declaration rather than a literal in a shipped `.sql` file —
`go run ./cmd/ragit-gen -dim 768` renders a set for a different width.

The generated models are exported. A read ragit does not offer can be written
with sqlb against `ragit.Document` and `ragit.Chunk` directly, inside
`ragit.WithTenant` so the RLS policies resolve.

## Chunks from somewhere else

If your extraction service also chunks and embeds — xberg does both on the same
`/extract` call — ragit does not need to redo either. `IngestPrepared` is
`ProcessDocument`'s sibling: same starting point, same terminal states, same
events, same resume guard, only the front half differs.

```go
documentID, err := processor.CreateDocument(ctx, in)
// ... your own extract / chunk / embed ...
err = processor.IngestPrepared(ctx, documentID, in.TenantID, ragit.PreparedDocument{
    Text:  extracted.Text,
    Space: embed.Space{Provider: "xberg", Model: "bge-base-en-v1.5", Dimension: 768},
    Chunks: []ragit.PreparedChunk{
        {Content: "...", Embedding: vec, HeadingPath: []string{"Handbook", "Accounts"}},
    },
})
```

A Processor for this path takes `nil` for the extractor, chunker and embedder —
it runs none of them. `embed.Space` is a struct rather than a fingerprint
string because retrieval filters on that fingerprint: a corpus written under a
string that disagrees by one character with what the query embedder reports
returns nothing, and says nothing.

Writing the chunk rows yourself with sqlb also works, and stays supported. It
means owning four things that are silent when forgotten: the scope, attribute
and expiry columns each chunk carries denormalized; the fingerprint; the
document's terminal state; and the `EventSink` notification. If you go that
way, `ragit.ResumeChunks` is the one piece not worth re-implementing — it takes
your executor, reports which chunks are already stored in the space you name,
and clears the document when any of them disagrees. Without it a bypassing
writer re-embeds the whole document on every retry, which costs money rather
than tidiness.

## Development

```
make test        # everything, needs Docker
make test-fast   # -short, no Docker
make generate    # regenerate migrations and models
```
