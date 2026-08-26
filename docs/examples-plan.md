# Plan: two runnable examples

Two example applications, built to answer one question: **how much of the
pipeline should xberg own, and does ragit's current API let a consumer choose?**

§5 of [`design.md`](design.md) settled that question on paper — xberg extracts,
Go chunks and embeds — but settled it by reasoning about one production system.
These examples are the experiment that either confirms the shape or exposes
where the API forces the answer rather than offering it.

- **`examples/extract-only/`** — xberg extracts; ragit chunks, embeds, stores,
  searches. The shipped happy path. Should build with **zero library changes**.
- **`examples/xberg-owned/`** — xberg extracts *and* chunks *and* embeds in one
  call; ragit only stores and searches. Currently **cannot be written against
  `Processor`** — see "The seam problem".

Both run against the same `examples/compose.yaml` (Postgres+pgvector, xberg,
MinIO) so every difference between them is pipeline shape, not infrastructure.

## What xberg actually exposes

Verified against the public docs and then against a running container —
**xberg 1.0.14**, not the `1.0.0-rc.42` [`design.md`](design.md) §4 was written
against. Re-check on upgrade.

- `POST /extract` is the **only** extraction endpoint. There is no `/chunk` and
  no `/embed`. Chunking and embedding are **configuration on the extract call**,
  carried in its `config` JSON parameter, not separate services.
- With chunking enabled the response carries chunks with `content`,
  `metadata.chunk_index`, `metadata.total_chunks`, `metadata.heading_path`,
  `metadata.page_spans`, `metadata.byte_start`/`byte_end` — and, with embedding
  configured, an `embedding` vector **per chunk**.
- Chunker config: `chunker_type` (`text`|`markdown`|`yaml`|`semantic`),
  `max_characters` (1000), `overlap` (200), `prepend_heading_context` (false),
  `table_chunking` (`split`|`repeat_header`).
- Embedding config: `model` — local ONNX presets `fast` (MiniLM, 384),
  `balanced` (BGE-base, 768), `quality` (BGE-large, 1024), `multilingual`
  (e5-base, 768) — or a hosted model through liter-llm, e.g.
  `openai/text-embedding-3-small`.
- Other endpoints, read from the running server's own `/openapi.json` rather
  than from the docs, because the two disagree: `/health`, `/version`, `/info`,
  `/detect`, `/formats`, `/cache/*`, plus `PUT /process` and
  `POST /v1/convert/file`. The `/extract-async` + `/jobs/{id}` pair the docs
  describe **is not there** on 1.0.14. Nothing in the plan depends on it, but it
  is a reminder to trust the container over the documentation.
- **No vector store, no search.** Retrieval is ragit's job in both examples.
  "Everything by xberg" therefore has a hard ceiling: extract + chunk + embed,
  never store + search.
- **No standalone text-embedding endpoint over HTTP.** `embed_texts()` exists
  as an in-process library function in the language bindings only. This is the
  detail that makes example B awkward — see below.

### The verified `/extract` contract

`/extract`'s request body is declared as bare `multipart/form-data` with no
schema, and there is no `ExtractionConfig` among the 90 component schemas — so
the config shape is not discoverable from the API description at all. It was
recovered by asking the CLI (`xberg extract --help`, then feeding it a bad key
so the deserializer enumerated the valid ones) and then confirmed over HTTP.

**This works, and it is what example B is built on:**

```bash
curl -X POST http://localhost:8234/extract \
  -F "files=@handbook.md;type=text/markdown" \
  -F 'config={"output_format":"markdown","chunking":{"max_chars":1000,"overlap":200,"chunker_type":"markdown","embedding":{}}}'
```

- The config field is `config`, alongside `files`. Top-level keys are
  `output_format`, `chunking`, `ocr`, `images`, `pdf_options`, `layout`,
  `pages`, `keywords`, `language_detection`, `token_reduction`, `use_cache`,
  `extraction_timeout_secs` and ~25 more. **`content_format` is not one** —
  `output_format` is, which is what ragit's `XbergExtractor` already sends.
- `chunking` takes **`max_chars`**, `overlap`, `chunker_type`
  (`text`|`markdown`|`yaml`|`semantic`), and `embedding`. Note `max_chars`, not
  the `max_characters`/`chunker_type` pairing the published docs list — the docs
  are wrong on this, and a request using them is accepted with the field
  ignored rather than rejected.
- **`"embedding": {}` alone turns embeddings on**, with the default local ONNX
  preset. No key, no model name, no provider needed. It downloads
  `xberg-io/embedding-models` from HuggingFace on first use (slow, cached in the
  container volume) and produces **768-dimension** vectors — BGE-base, the
  `balanced` preset, exactly the width this plan assumed.
- The response envelope is `{results: [...], summary: {...}}`, and each result
  carries `chunks: [{content, chunk_type, embedding, metadata}]` with
  `metadata` = `byte_start`, `byte_end`, `chunk_index`, `total_chunks`,
  `heading_context`. No `page_spans` or `token_count` unless page tracking and
  token sizing are turned on.
- `heading_context` is `{"headings":[{"level":1,"text":"…"},…]}` — a level+text
  trail, strictly richer than ragit's `HeadingPath []string`, which it maps onto
  by taking each `.text`.
- `chunk_type` classifies each chunk (`heading`, `unknown`, …). ragit has no
  column for it; it would go in the chunk's `metadata` JSONB.

### The two chunkers disagree, and that is the comparison

Same document (`fixtures/handbook.md`), same settings — 1000 characters,
200 overlap:

| chunker | chunks |
|---|---|
| ragit `chunk.Chunker` | 13 |
| xberg `chunker_type: markdown` | 5 |

xberg packs sibling sections together up to the budget; ragit splits per heading
section. Neither is wrong, but they produce meaningfully different retrieval
granularity from identical input, and B will make that visible in the search
results rather than leaving it as a number. (At small `max_chars` xberg also
emits bare heading-only chunks — three of the first five at `max_chars: 120` —
each costing an embedding. Not a problem at realistic sizes, but a reason not to
tune that knob down thoughtlessly.)

## The seam problem

Example B does not fit the current API, and that is the finding, not a blocker
to engineer around quietly.

- `extract.Extractor` returns `Result.Text` — one flat string
  ([extract/extract.go](../extract/extract.go)). Chunks have nowhere to go.
- `Processor` holds a **concrete** `*chunk.Chunker`, not an interface
  ([ragit.go:114](../ragit.go)), and `extractAndChunk` always calls
  `SplitMarkdown` on the extractor's text ([ragit.go:436](../ragit.go)).
- `embedAndStore` always calls `p.embedder.Embed` ([ragit.go:452](../ragit.go)).

So there are exactly three ways to write B, and the example should be built the
third way:

1. **Flatten** — have the xberg extractor join chunks back into `Text` and let
   ragit re-chunk and re-embed. Wastes everything xberg did. Not an example of
   anything.
2. **Change the library first.** Premature: we do not yet know which seam is
   the right one, which is what the example is for.
3. **Bypass `ProcessDocument`.** Use `Processor.CreateDocument` for the row and
   the object-storage write, then insert `ragit.Chunk` rows directly with sqlb
   inside `ragit.WithTenant`, then search with `Processor.VectorSearch`. The
   README already advertises exactly this escape hatch ("a read ragit does not
   offer can be written with sqlb against `ragit.Document` and `ragit.Chunk`").
   **B is the test of whether that escape hatch also works for writes.** If it
   is unbearable, we have learned what seam to add and why.

### Query-time embedding is the harder half

`VectorSearch` embeds the query with `p.embedder` and filters chunks on
`embedding_fingerprint` ([search.go](../search.go)). In B the corpus was
embedded by xberg, so the query must land in xberg's embedding space or the
filter excludes every row it just wrote.

Since xberg has no HTTP embedding endpoint, B must implement `embed.Embedder`
by POSTing the query text to `/extract` as a one-chunk pseudo-document. That is
genuinely ugly, and writing it is the point: it is the sharpest available
evidence about whether "one `Embedder` serves both corpus and query" is the
right constraint.

Whatever B's embedder reports as `Provider()`/`Model()`/`Dimension()` must be
stable, because it is half of `embedding_fingerprint`. Proposal:
`xberg|bge-base-en-v1.5|768`.

## The dimension fork

`ragit-gen -dim N` bakes the width into the migration set, so the two examples
need **different generated migrations**:

- A: **1536** — EdenAI `google/gemini-embedding-001`, Matryoshka-truncated from
  its native 3072 by `embed.Client`. Matches `RAG_EMBEDDING_DIM=1536` in the
  existing project envs, so it is the real-world number.
- B: **768** — xberg's `balanced` ONNX preset (BGE-base), embedded locally in
  the sidecar with no external API and no per-token cost.

Choosing 768 for B is deliberate: it forces the consumer-side regeneration path
into daylight rather than letting both examples share ragit's shipped
migrations. If it proves intolerable, B can instead point xberg at
`openai/text-embedding-3-small` (1536) via liter-llm — but that quietly
reintroduces a hosted provider, which is the thing B exists to avoid.

**Open question this settles:** whether a consumer can realistically run
`ragit-gen`, or whether the vector column should be declared unconstrained with
a dimension check instead.

## Shared setup

`examples/compose.yaml`:

- `pgvector/pgvector:pg18`
- `ghcr.io/xberg-io/xberg:latest` running `xberg serve`
- `minio/minio`

`examples/internal/bootstrap/` (shared by both, ~100 lines):

- Applies **the host app's own** migrations, then `ragit.Migrate` — two
  independent migration lines, which is the collision story `ragit_` prefixing
  exists for.
- Declares a small host-app table with sqlb (`schema.NewModule("demo")` →
  `demo_uploads`) that carries an `upload_id` the app joins back to
  `ragit_documents.id`. This is what makes the examples *host applications*
  rather than CLI wrappers.
- **Creates and connects as an unprivileged role.** Non-negotiable: PostgreSQL
  exempts superusers from RLS, so an example connecting as the compose file's
  `POSTGRES_USER` would demonstrate isolation that isn't there. The role SQL is
  lifted from [`internal/testutil`](../internal/testutil/db.go).
- Calls `extract.RunIsolatedChildIfInvoked()` as the first statement in
  `main()` — A needs it for its fallback chain, and an example that omits it
  teaches the wrong wiring.

Config comes from the existing env layout (`~/.config/envs`), so the examples
read `EDENAI_API_KEY`, `EDENAI_BASE_URL`, `EDENAI_EMBEDDING_MODEL`,
`DATABASE_URL`, `XBERG_URL`. No key is ever written into the repo.

Fixtures: three or four small self-authored documents in `examples/fixtures/` —
a Markdown handbook with real heading depth (so `HeadingPath` has something to
carry), a CSV, and a text-layer PDF. Nothing copyrighted, nothing large.

## What each example must demonstrate

Same script, so the two are directly comparable:

1. Ingest the fixture set for tenant A, printing per-document status, chunk
   count, and `embedding_fingerprint`.
2. `VectorSearch` and `FullTextSearch` for one query, printing each hit's score,
   document filename, and heading path — proving citation metadata survives the
   round trip. **This is where A and B diverge most visibly**: A's heading paths
   come from `chunk.Chunker`, B's from xberg's `metadata.heading_path`.
3. Filter the same query by `Attributes` and show the result set narrow.
4. Search as tenant B and get nothing — RLS and `Scope` both live.
5. Re-run ingestion unchanged and show zero embedding calls the second time —
   A proves the resume guard; **B probably cannot**, because the resume logic
   lives inside `ProcessDocument`, which B bypasses. That asymmetry is a
   finding, and the example should print it rather than hide it.
6. Print total embedding calls made. EdenAI bills per call; an example that
   silently costs money on every run is a bad example.

## Module layout

`examples/` gets its **own `go.mod`** with `replace github.com/mind-vm/ragit
=> ../`. The examples need a River client, a MinIO client, and compose-only
tooling; folding those into the library's `go.mod` would misrepresent what
importing ragit actually costs. A consumer reading `go.mod` should see the
library's real dependency surface.

## Sequencing

1. ~~`examples/compose.yaml` + `bootstrap` + fixtures. Verify the unprivileged
   role actually has RLS applied before writing either example.~~ **Done** —
   `cd examples && make up && make verify`. Nine checks, all green, idempotent
   on re-run. Two things it caught that would otherwise have surfaced later:
   the PostgreSQL 18 images want their volume at `/var/lib/postgresql` rather
   than `.../data` or the container refuses to start, and this machine had the
   obvious host ports taken (hence 5455/8234/9200).
2. ~~**A first**, and note every place the README's wiring instructions turn out
   to be incomplete. A is a documentation test as much as a demo.~~ **Done** —
   `examples/extract-only`. See "What A found" below.
3. ~~Confirm the `/extract` chunk+embed config shape against the running
   container. If chunking-with-embeddings does not work over REST as documented,
   B changes shape and we should re-plan rather than improvise.~~ **Done, and B
   is a go** — extract + chunk + embed in one HTTP call, 768-dimension vectors,
   full heading trail. No re-plan needed. The verified contract is above.
4. ~~**B via the sqlb bypass.** Write down every friction point as it appears —
   that log is the actual deliverable.~~ **Done** — `examples/xberg-owned`. See
   "What B found" below.
5. ~~Decide whether A should also carry the River path
   (`CreateDocument` + enqueue + worker) or stay synchronous on `Ingest`.
   Recommend adding it: [`jobs/`](../jobs) has never been exercised from
   outside this module.~~ **Done, as a third program** — `examples/async`. See
   "What the queue found" below.

## What A found

`examples/extract-only` was written against the shipped API and **needed no
library changes**, which is the result the plan hoped for: the README's wiring
instructions are complete and correct, and a consumer can build this pipeline
from the outside. Four things it turned up anyway.

**Full-text search silently returns nothing for a natural-language question.**
ragit's `search_vector` is a `'simple'`-config `tsvector` queried with
`websearch_to_tsquery('simple', …)`. §9 chose `'simple'` deliberately —
language-agnostic, no stemming — but `'simple'` also has no *stopword*
dictionary, and `websearch_to_tsquery` ANDs every surviving token. Verified
against the running database:

```
to_tsvector('simple',  'how do I reset my password')
  → 'do':2 'how':1 'i':3 'my':5 'password':6 'reset':4
websearch_to_tsquery('simple', 'how do I reset my password?')
  → 'how' & 'do' & 'i' & 'reset' & 'my' & 'password'
to_tsvector('english', 'how do I reset my password')
  → 'password':6 'reset':4
```

So `FullTextSearch` asks for a chunk containing "how" and "do" and "i", finds
none, and returns an empty slice — indistinguishable from an empty corpus. The
same question through `VectorSearch` returns five good hits. This is not a bug,
but it is an unwritten precondition on a public method, and the README's own
retrieval example is a natural-language question. **Shape question 7: should
`FullTextSearch` document this, strip stopwords, or use `OR` semantics?**

**Non-Markdown documents lose their citation trail.** The CSV and the PDF each
produced one chunk with an empty `HeadingPath`, because ragit's chunker derives
the trail from Markdown headings and neither document has any — xberg renders
the CSV as a Markdown *table*, which has no headings at all. A citation UI gets
"(no heading)" for every non-Markdown source. Worth holding onto for the
comparison: xberg's own chunker carries `heading_path` and
`prepend_heading_context`, so B may well do better here, on the very axis §5
used to justify keeping the chunker in Go.

**`documents.embedding_model` is not the fingerprint.** It stores
`embedder.Model()` alone (ragit.go:311) while each chunk stores the full
`provider|model|dimension`. So the document-level column cannot distinguish two
providers serving the same model name, nor two dimensions of one — which is
exactly the straddled state it appears to report on. `CountMisalignedChunks` is
the read that actually answers it. **Shape question 8: should that column carry
the fingerprint instead?**

**`DATABASE_URL` is a loaded gun for a consumer's examples.** Every env file in
`~/.config/envs` sets one, the environment beats a `.env`, and `bootstrap`
creates roles and tables — so sourcing a project env file to pick up the EdenAI
key would have pointed the examples' migrations at a real database. Bootstrap
now refuses any database not named `ragit_examples`. Nothing for the library to
fix, but it says something about how the README should phrase credential setup.

Confirmed working, for the record: the resume guard (a second pass over
unchanged documents makes **zero** embedding calls), attribute narrowing
(5 results → 2 under `team=warehouse`), tenant confinement (tenant B sees
nothing), and the three-layer extractor chain with
`RunIsolatedChildIfInvoked()` wired from `main()`.

## What B found

`examples/xberg-owned` runs: one `/extract` call returns each document
extracted, chunked and embedded at 768 dimensions, and ragit stores and searches
it. Vector search returns sensible hits, attribute narrowing works, and tenant B
sees nothing. **The sqlb bypass is viable.** It is also expensive in a way that
answers most of the open questions.

### What the bypass costs

Everything `ProcessDocument` does for free and the bypass has to redo. Three of
the six fail *silently* when got wrong, which is the part that matters.

**0. A Processor cannot be built without dependencies this path never uses.**
`ragit.New` demands an `extract.Extractor` and a `*chunk.Chunker`. B needs a
Processor for `CreateDocument`, `GetDocument`, `ListDocuments`, `VectorSearch`,
`FullTextSearch` and `DeleteDocument` — the whole catalog and retrieval surface
hangs off the same struct as ingestion — so it passes `nil, nil` and relies on
knowing which methods stay away from them. That is a memorised fact about the
implementation, not a contract.

**1. Denormalization is by hand, and silent.** `tenant_id`, `scope_a_id`,
`scope_b_id`, `session_id`, `attributes` and `expires_at` all have to be read
back off the document and copied onto every chunk. Forget `attributes` and
attribute filtering returns nothing; forget `scope_a_id` and the chunks answer
searches for the wrong scope. Neither errors.

**2. The embedding fingerprint is the caller's to get right, and silent.** It is
what `VectorSearch` filters on, so a mismatch between what ingestion writes and
what the query embedder reports produces zero results — indistinguishable from
an empty corpus. B uses one object for both so they cannot drift, which is a
discipline the API does not enforce.

**3. The resume guard is gone.** Measured: a second pass costs **3 extract calls
and 7 chunks re-embedded**, where `extract-only` shows **0**. The check lives
inside `embedAndStore`, which this path never reaches. Local ONNX makes that CPU
rather than money — but a hosted embedding model on this path would be re-billed
on every retry, which is the exact incident §7 exists to prevent.

**4. The terminal state is hand-written, and silent.** `Processor.finish` is
unexported, so `status`, `text_content`, `metadata`, `chunk_count`,
`embedding_model` and `processed_at` are all set again from outside with nothing
checking the set is complete. Miss one and the catalog quietly disagrees with
the chunks table.

**5. The EventSink never fires.** A host application subscribing to indexing
events simply stops hearing about documents that came in this way.

### What survives the bypass, and why that is the interesting half

Tenant confinement holds — tenant B gets nothing — because RLS is the
*database's*, not the Processor's. Full-text search works without the bypass
doing anything at all, because `search_vector` is `GENERATED ALWAYS AS`. Both
are properties pushed down into the schema rather than enforced in Go, and both
are exactly the properties that survived code that went around the library. That
is an argument for where to put the rest of them.

### The dimension fork, priced

Worse than expected, and it is the migration story rather than the schema:

- B needs **its own database** (`ragit_examples_768`). A vector column's width
  is part of its type, so 768 and 1536 corpora cannot share tables.
- **`ragit.Migrate` is unusable at any other dimension.** It applies migrations
  embedded in the library. `go run ./cmd/ragit-gen -dim 768` renders a correct
  set — that part works cleanly — but *applying* it is entirely the consumer's
  problem: `internal/migrate` is internal, `ragit.Migrate` is not parameterised
  by a filesystem, and `internal/migrate.TableName` is not exported, so the
  consumer re-implements the goose runner and has to *remember* the
  `ragit_migrations` table name. Get it wrong and goose silently starts a second
  history in `goose_db_version`.
- The hand-composed RLS and `tsvector` changes live in **package main** inside
  `cmd/ragit-gen`, so nothing can import them. Fine as long as the generator is
  the only entry point; a blocker for anyone wanting to compose them.
- The generated **models** need nothing: `Chunk.Embedding` is `*sqlb.Vector`
  whatever the width. Only the SQL differs.

### Query-time embedding: the awkwardness is the evidence

It works — the query is posted to `/extract` as a one-chunk pseudo-document and
the vector read off the chunk — at the cost of a full extraction round trip
(MIME detection, format dispatch, chunking) to embed six words.

Worse, `Provider()`/`Model()`/`Dimension()` are **asserted, not observed**:
xberg's response says nothing about which model produced the vectors, not in the
chunk and not in the result metadata. Change the preset and the fingerprint goes
on claiming BGE-base while the corpus straddles two spaces looking like one.

### Measurement moved

`extract-only` counts embedding work by decorating `embed.Embedder`, because
every vector there passes through it. On this path the corpus **never touches an
Embedder** — vectors arrive as a side effect of extraction — so the same
decorator reports almost nothing. B counts on the HTTP client instead. Any
cost-accounting ragit grows has to reckon with both shapes.

### One guess in this plan was wrong

It speculated that xberg's chunker might carry citation metadata for
non-Markdown documents where ragit's cannot. **It does not.** The CSV and the
PDF produce `(no heading)` in B exactly as in A — xberg's `heading_context` is
derived from Markdown headings too. The §5 argument for keeping the chunker in
Go is unaffected on that axis.

### Retrieval, compared

Same document, same 1000/200 budget:

| | chunks from `handbook.md` | top hit's heading trail |
|---|---|---|
| A (ragit chunker) | 13 | `… › Accounts › Resetting a password` |
| B (xberg chunker) | 5 | `… › Accounts` |

B's citations are coarser because its chunks are bigger — a chunk spanning a
whole `## Accounts` section cites that section, not the subsection that actually
answered. If a citation UI is the requirement, that is a point for A, and it is
the concrete version of the argument §5 made from reasoning alone. (Scores are
not comparable across embedding spaces; do not read anything into 0.6691 vs
0.6513.)

### The shape questions, answered

1. **Chunker as an interface?** *No — it does not help.* B does not want a
   different chunker, it wants no chunking step at all. An interface would still
   sit downstream of an Extractor that returns flat text. The needed seam is
   earlier.
2. **A seam for "chunks and vectors already exist"?** *Yes.* This is the finding.
   The bypass is workable but redoes six things, three of them silently. An
   `IngestPrepared`-shaped entry point that owns the denormalization, the
   fingerprint stamping and the terminal state would remove all of 1, 2, 4 and 5.
3. **One Embedder or two?** *At minimum, an Embedder must not be mandatory for
   ingestion.* B supplies one solely so `VectorSearch` can embed a query, and
   pays a pseudo-document hack for it. Corpus-embedding and query-embedding are
   genuinely different jobs here.
4. **Is `-dim` at generation time survivable?** *The generator is; the runner is
   not.* Cheapest real fix: export the migration runner parameterised by an
   `fs.FS`, or at least export the version-table name.
5. **Ship the unprivileged-role SQL?** *Still open* — unchanged by B, though
   both examples now carry the same 20 lines.
6. **Resume guard lower down?** *Yes, and it is the single most valuable thing
   to move.* It is the one property whose absence costs money rather than
   tidiness.

## What the queue found

`examples/async` runs the extract-only pipeline entirely through River: nothing
is processed inline, all three workers are registered, and the retention sweep
is scheduled periodically. It is a **third program** rather than a flag on
`extract-only` — the point of A and B is that they read side by side and differ
only in pipeline shape, and a River client, a subscription and a shutdown
sequence in one of them would wreck that comparison.

Everything in [`jobs/`](../jobs) works. Documents reach `ready` through
`ragit_process_document` jobs, `DeleteDocumentWorker` removes a document
through the queue, and the sweep deletes an expired document across tenants
under `WithMaintenance` — the only path in ragit that reads cross-tenant, and
the only user of that escape hatch, so it had never been exercised end to end.

### It also found a bug, and this is why step 5 was worth doing

**`jobs.DeleteExpiredArgs` could only ever run one retention sweep per day.**
Fixed in this branch.

`InsertOpts` set `UniqueOpts{ByArgs: true}` and left `ByState` unset. The args
are an empty struct, so `ByArgs` makes every instance identical — and River's
default `ByState` **includes `JobStateCompleted`**, documented in River's own
source: "if a unique job has `completed`, you still can't insert a duplicate, at
least not until the job cleaner maintenance process eventually removes the
completed job". River's default retention for completed jobs is 24 hours.

So the intent in the code comment — "one sweep in flight at a time" — was not
what the code did. What it did was: the first sweep runs, and every subsequent
insert is silently skipped for a day. Nothing errors, because a skipped unique
insert is a *success* that returns the existing job. A deployment following the
package's own suggested wiring (`river.PeriodicInterval(15*time.Minute)`) would
have got one sweep per day and no signal that anything was wrong. Retention
would simply have lagged, unboundedly, on a library whose whole point is that
its ephemeral-attachment scope expires.

It was invisible from inside the module because nothing there ever inserted the
job twice. `examples/async` found it on its **second run**: the first run
passed, the second timed out waiting for a document to be swept.

The fix names the non-terminal states explicitly (`available`, `pending`,
`running`, `retryable`, `scheduled` — the first four are required by River
whenever `ByState` is set), with a regression test in `jobs/args_test.go` that
asserts `completed` is absent.

**Upgrade note for any existing deployment:** River evaluates uniqueness against
the `unique_states` stored on the *existing* row, so a completed sweep inserted
before this fix goes on blocking new ones until the job cleaner removes it. That
was observed directly — the fix appeared to do nothing until the stale row was
deleted. Clearing it is a one-liner:

```sql
DELETE FROM river_job WHERE kind = 'ragit_delete_expired' AND state = 'completed';
```

### Two smaller frictions

**Grants do not cover tables that do not exist yet.** `GRANT … ON ALL TABLES`
expands at the moment it runs. `bootstrap.Setup` grants after ragit's
migrations, River's migrations then create `river_*`, and the application role
is locked out of them — a bare permission error on the first job insert, at
runtime, far from the cause. `examples/async` re-grants after migrating River,
and a real deployment needs either that discipline or `ALTER DEFAULT
PRIVILEGES` up front. Worth a line in ragit's own deployment notes, since the
same trap applies to ragit's tables if a host app grants before migrating.

**The queue name is load-bearing and silent.** ragit's job args name the queue
`ragit_process_document`. A host application whose River client does not
configure that queue registers the workers successfully, inserts jobs
successfully, and then nothing ever runs them. Nothing reports a problem.

### A finding about the host application, not the library

`demo.EnsureDocument` deduplicates uploads against the application's own table.
When a document is deleted *through ragit* — by the sweep, or a
`DeleteDocumentWorker` job — that row is left pointing at nothing, because ragit
has no idea the table exists. The fixture then looks "already indexed" while
nothing is indexed. The helper now verifies the pointer and repairs it, but the
real answer is the `EventSink`: an application that wants to stay consistent
with ragit's catalog has to subscribe, not poll. That is an argument for the
sink being on *every* terminal state, which it already is — and against the
bypass in B, where it never fires at all.

## Shape questions these are meant to answer

Recorded up front so the examples are judged against them rather than
rationalised afterwards:

1. Should `Processor` take a chunker **interface** instead of `*chunk.Chunker`?
2. Is there a legitimate seam for "chunks and vectors already exist" — an
   `Extractor` that may return chunks, or a separate `IngestPrepared` entry
   point — or is the sqlb bypass genuinely good enough?
3. Should corpus-embedding and query-embedding be one `Embedder` or two? The
   fingerprint says one; B's pseudo-document hack is the argument for two.
4. Is `-dim` at generation time survivable for a consumer?
5. Should ragit ship the unprivileged-role SQL (a `ragit.GrantAppRole` helper or
   a documented snippet) rather than leaving every consumer to rediscover the
   superuser/RLS trap?
6. Does the resume guard belong to `ProcessDocument`, or lower down where a
   bypassing caller could still use it?
7. Should `FullTextSearch` document, or fix, the fact that a natural-language
   question matches nothing under the `'simple'` config? *(raised by A)*
8. Should `documents.embedding_model` carry the full fingerprint rather than the
   model name? *(raised by A)*

## Sources

- [xberg (GitHub)](https://github.com/xberg-io/xberg)
- [xberg docs](https://docs.xberg.io/)
- [API server guide](https://docs.xberg.io/guides/api-server/)
- [Chunking guide](https://docs.xberg.io/guides/chunking/)
- [Embeddings guide](https://docs.xberg.io/guides/embeddings/)
