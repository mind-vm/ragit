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

Confirmed from the running server's schema: the `Chunk` object carries an
`embedding` field, "only populated when `EmbeddingConfig` is provided in
chunking configuration" — so extract + chunk + embed really is one call, and
example B's premise holds.

Still unverified, and the reason step 3 exists: `/extract`'s request body is
declared as bare `multipart/form-data` with no schema, so **the exact JSON shape
of the `config` field is not discoverable from the API description** and has to
be probed. Likewise whether `heading_path` survives the `markdown` chunker for
the formats we care about.

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

`examples/` gets its **own `go.mod`** with `replace github.com/jryannel/ragit
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
3. Confirm the `/extract` chunk+embed config shape against the running
   container. If chunking-with-embeddings does not work over REST as documented,
   B changes shape and we should re-plan rather than improvise.
4. **B via the sqlb bypass.** Write down every friction point as it appears —
   that log is the actual deliverable.
5. Decide whether A should also carry the River path
   (`CreateDocument` + enqueue + worker) or stay synchronous on `Ingest`.
   Recommend adding it: [`jobs/`](../jobs) has never been exercised from
   outside this module.

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
