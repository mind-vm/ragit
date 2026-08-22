# ragit — RAG library design using xberg (formerly kreuzberg)

Status: draft, revised against a real reference implementation
Audience: internal, for reuse across multiple SaaS projects
Stack assumptions: Go, sqlc, PostgreSQL (+pgvector), River (job queue), S3-compatible object storage, Markdown as canonical report/output language

**Note on sourcing**: §§6–9 were revised after reading `valiro-go` (`internal/rag/`), a production Go codebase running on this exact stack (sqlc, pgvector, River, S3). Where its choices differ from the first draft of this doc, that's called out explicitly, with the incident or reasoning that drove it — several of them overturn what was originally recommended here.

**Adoption history matters for how to read this.** `valiro-go` did **not** start with xberg. The timeline, from its own commit history:

- **2026-01-06** — original RAG stack built: own parsers (`ledongthuc/pdf`, `excelize`, `encoding/csv`), own hand-rolled chunker, own Gemini-based embedder. No xberg involved at all.
- **~6.5 months of production use later, 2026-07-22** — the OOM incident described in §6 (a 212 kB PDF driving one parser to ~5 GB, "the tenth such kill in 30 days"). Fixed with `IsolatedExtractor`, a memory-capped child process — a hardening layer bolted onto the *existing* local-parser stack.
- **2026-07-27, five days later** — xberg adopted as an *additive* sidecar (`feat(rag): extract documents in an Xberg sidecar instead of in-process`), gated behind `XBERG_URL`, with the local-parser + isolated-child stack kept intact as the fallback.

So the three-layer extractor chain in §6 (xberg → isolated child → raw parser) is the shape of an **incremental retrofit**, not a from-scratch design: containment was built to survive a stack that didn't yet have xberg, and xberg was layered on top afterward without removing that containment. The underlying lessons (process isolation matters, narrow fallback semantics, own the chunker/embedder) are still sound and worth keeping — but a `ragit` built fresh, knowing xberg exists from day one, doesn't have to reconstruct this history to get the same safety. See the design note at the end of §6 for what that implies concretely.

## 1. What xberg actually gives us

Kreuzberg (the Python doc-extraction library by Goldziher) and **xberg** (github.com/xberg-io/xberg, MIT) are, per xberg's own docs, the same lineage: "Xberg is the next iteration of Kreuzberg... same document-intelligence engine, rebuilt and rebranded under a fresh v1 line." It's now a Rust-core engine with 15 language bindings including Go, plus a CLI, REST API, and MCP server. This matters for the design: **the engine is not Python-only anymore**, so we're not forced into a Python microservice — Go can talk to it natively (cgo) or over HTTP.

Relevant capabilities:

- **Formats**: ~101 formats / 115 extensions — PDF, Office (docx/xlsx/pptx + odt/ods/odp), images, audio/video (Whisper transcription), HTML/XML/JSON/YAML/CSV, email (EML/MSG/PST), archives (recursive), LaTeX/Jupyter/BibTeX. Plus code intelligence for 371 languages via tree-sitter (functions, classes, symbols, docstrings) — useful if any SaaS product needs to RAG over a customer's codebase, not just documents.
- **OCR**: pluggable backends — Tesseract, PaddleOCR (ONNX), Candle (pure Rust CPU), and VLM-based OCR (GPT-4V/Claude/Gemini) with fallback chains, per-page auto-detection (only OCRs pages lacking a text layer), language auto-detect, DPI control (150/300/600).
- **Tables**: ML layout + table-structure models reconstruct reading order and cell grids into clean Markdown tables — the single most valuable feature here, since hand-rolled table-to-Markdown is usually the worst part of a DIY pipeline. `valiro-go`'s own local xlsx/csv parsers do this by hand today (see §5) — xberg replaces that for every format except spreadsheets, where it's deliberately *not* used (§5 explains why).
- **Output formats**: plain text, **Markdown** (RAG-friendly, matches our report-language convention — and confirmed as what `valiro-go` actually requests: `output_format: "markdown"`, "chunks are embedded and fed to the model as context, so retaining headings and table structure is worth the few extra tokens"), Djot, HTML, JSON.
- **Chunking & embedding**: also built in (`xberg chunk`, `xberg embed`) — but **not used** by the one production system we've checked. See §5.
- **Safety limits**: zip-bomb / compression-ratio / nesting-depth guards, per-file timeouts.
- **Deployment shapes**:
  - Go library via cgo/FFI: `import "github.com/xberg-io/xberg/packages/go"` — statically links, in-process, no network hop.
  - REST server: `xberg serve --host 0.0.0.0 --port 8000`. Docker image `ghcr.io/xberg-io/xberg:latest`. **This is the shape actually running in production** — see §4.
  - CLI, and an MCP server (`xberg mcp`) exposing extraction as agent tools.

Given this, xberg *can* plausibly own three stages of the pipeline (extract, chunk, embed). What a real deployment actually does with that is narrower — see §5.

## 2. Reference pipeline

```
Upload → Object Storage (S3) → River job → xberg extract (with fallback) → chunk (Go) → embed (Go, external API)
      → pgvector store → retrieval API
```

1. **Ingest**: app uploads to S3 (bucket per env, prefix per tenant), writes a `documents` row (`status = pending`), enqueues one River job.
2. **Extract**: the job calls the extractor stack described in §6 — xberg first, with a narrow, evidence-based fallback chain — and gets Markdown + metadata back.
3. **Chunk**: split the Markdown in Go (heading-aware for Markdown, recursive-character-split with overlap otherwise). **Not** delegated to xberg — see §5.
4. **Embed**: batch-embed chunks via a single OpenAI-wire-compatible embeddings client, called from Go — default backend EdenAI's `/v3/embeddings` gateway. Provider/base-URL/key is per-tenant config.
5. **Index**: insert chunks + vectors into Postgres/pgvector.
6. **Retrieve**: vector search (and, separately, full-text search) with tenant/scope filtering, returns chunks with citations.

The one thing this section originally got wrong: **it's one job, not a chain of three.** See §7 for why, and what actually goes wrong when you split extract/chunk/embed into separately-retried River jobs.

## 3. Design variation A — xberg as embedded Go library (cgo)

River worker links `xberg-io/xberg/packages/go` directly and calls `xberg.Extract()` in-process.

```go
func (w *ExtractWorker) Work(ctx context.Context, job *river.Job[ExtractArgs]) error {
    data, err := w.s3.GetObject(ctx, job.Args.Bucket, job.Args.Key)
    input := xberg.ExtractInputFromBytes(data, job.Args.MimeType, &job.Args.Filename)
    out, err := xberg.Extract(*input, xberg.ExtractionConfig{
        Ocr: &xberg.OcrConfig{Backend: "tesseract", Language: []string{"eng", "deu"}},
    })
    ...
}
```

**Pros**: no network hop, no extra service to deploy/monitor, simplest ops story, works offline/air-gapped, cheapest at low-to-medium volume.
**Cons**: cgo build complexity (cross-compilation and multi-arch Docker builds get harder), OCR/ML workloads run inside the same process as your job worker — and §6 documents a real production OOM incident from exactly this shape of risk (parser inside the app process, no memory ceiling), which is the strongest argument against this variation for anything handling untrusted uploads.

**Best for**: a single SaaS, low volume, fully offline/air-gapped requirement, team willing to own the resulting blast radius.

## 4. Design variation B — xberg as a sidecar REST service (recommended, and what's actually running)

Deploy `ghcr.io/xberg-io/xberg:latest` running `xberg serve` as its own service. Go workers call it over HTTP.

This is exactly what `valiro-go` runs, and its real client is worth looking at directly rather than a sketch — it's a good template:

```go
// POST /extract, multipart, field "files", plus an "output_format" field.
// Response is one JSON envelope: {results: [...], errors: [...], summary: {...}}.
// A per-file failure comes back as HTTP 200 with an empty results[] and an
// entry in errors[] — status code alone does not tell you extraction failed.
func (e *XbergExtractor) extract(ctx context.Context, data []byte, filename string) (*ExtractionResult, error) {
    body, contentType, _ := xbergMultipart(data, filename) // field "files" + "output_format=markdown"
    req, _ := http.NewRequestWithContext(ctx, http.MethodPost, e.BaseURL+"/extract", body)
    req.Header.Set("Content-Type", contentType)
    resp, err := e.client().Do(req)
    if err != nil {
        return nil, fmt.Errorf("%w: %v", ErrXbergUnavailable, err) // connection refused, DNS, timeout
    }
    // resp.StatusCode >= 500  → treat as ErrXbergUnavailable (server broke, not the file)
    // resp.StatusCode != 200  → a verdict on the document (400 validation, 422 parsing/OCR) — no fallback
    ...
}
```

Real API quirks worth knowing before you build against it (verified against xberg `1.0.0-rc.42`, worth re-checking on upgrade):
- `page_count` lives at `metadata.format.page_count`, not the top-level field some published examples suggest.
- A 200 response can still carry a failed extraction — always check `errors[]`, not just the status code.

**Pros**: clean process isolation, independently scalable (OCR is CPU-heavy when enabled — see §5 for why it's CPU-only and opt-in — so the sidecar can get its own pool sized to that load without touching the rest of the worker fleet), independently upgradable, reusable across every SaaS project and every language — this is the one that best matches "a library for several SaaS projects." Also unlocks the MCP server mode for agent tool-use for free.
**Cons**: network hop + multipart handling for large files, one more service to deploy/monitor/secure.

**Make it optional, not required.** `valiro-go` gates the whole sidecar behind an env var (`XBERG_URL`) — unset keeps prior (local-parser-only) behavior exactly. No deployment is forced to run it, and startup does a non-fatal health check (`GET /health`) purely to log a clear signal instead of discovering a missing sidecar on the first upload. Carry this into `ragit`: an `Extractor` should degrade gracefully to "smaller supported format set, no OCR" rather than fail to construct when no xberg URL is configured.

**Best for**: the default, for every project using this library, unless there's a specific offline/air-gapped reason not to.

## 5. Design variation C — how much of the pipeline should xberg own?

The original framing here was "xberg does extract + chunk, Go does embed." **That's not what the reference implementation does, and the reason is instructive.**

`valiro-go` uses xberg for extraction *only*. Chunking is a ~150-line hand-rolled Go package (`chunker.go`): a Markdown-header-aware splitter that falls back to recursive-separator splitting (`\n\n`, `\n`, `. `, ` `, then a hard byte-boundary split) with configurable overlap. It is **not** calling `xberg chunk` at all, despite that existing.

Why keep chunking in Go when xberg would do it:
- Chunk metadata (`Section`, `HeaderLevel`, `CharStart`/`CharEnd`) needs to round-trip into your own citation UI and your own `document_chunks` schema. That coupling is tighter and simpler to own directly than to reverse-engineer from a sidecar's chunk output.
- Markdown-aware recursive splitting is a well-understood, small algorithm — not a place where outsourcing saves meaningful engineering time, unlike OCR/table-reconstruction which genuinely is.
- One embedding-cost lever (chunk size/overlap) staying in Go alongside the other (embedding provider — see below) keeps both cost/quality knobs in one place instead of split across a sidecar config and app config.

Embedding follows the same logic, for a different reason: embedding provider choice is a per-tenant business decision (cost tier, data residency, "bring your own key"). That has to live in `ragit`'s own config layer regardless of where chunking happens.

**Decision for this library, now backed by a working system rather than just reasoning about it**: xberg does extraction only. Chunking and embedding are both Go-owned. This also keeps the sidecar stateless and cacheable — same document + same extract config → same output, regardless of chunking strategy or which tenant's embedding provider is in play.

One caveat: this was one system's choice, not a law of nature. If a future `ragit`-using project has no citation UI and no per-tenant chunk-size tuning need, letting xberg do markdown-mode chunking too is a legitimate simplification — just don't reach for it as the default.

### Embedding: one client, OpenAI-compatible wire format

`embed/` is **not** a set of bespoke per-provider adapters (OpenAI schema, Voyage schema, Cohere schema...). It's a single HTTP client that speaks the OpenAI embeddings wire format — `POST /embeddings` with `{model, input, dimensions, encoding_format}`, response `{data: [{index, embedding}]}` — parameterized by base URL, API key, and model name. This works because that shape is a de-facto standard: OpenAI's own API, and any "OpenAI-compatible" gateway in front of other providers, speak it identically.

The default configuration points this client at **EdenAI's EU gateway** (`https://api.eu.edenai.run/v3/embeddings`, confirmed genuinely OpenAI-schema-compatible on the wire, currently proxying `google/gemini-embedding-001`), which is what `valiro-go` runs today. Pointing the same client at OpenAI directly, or at any other OpenAI-compatible gateway, is a config change, not a code change — that's the entire value of standardizing on this wire shape instead of writing a provider-specific adapter per name.

Two real quirks worth carrying over into `ragit`'s client, both learned the hard way in `valiro-go`:
- **Don't trust the `dimensions` request field blindly.** EdenAI doesn't reliably honor it for `gemini-embedding-001` — it returns the native 3072-width vector regardless. The client must defensively check the returned length and, if longer than requested, **Matryoshka-truncate and L2-renormalize** to the target width (valid for MRL-trained embeddings like Gemini's; not valid for every model, so this needs to be a documented, opt-outable behavior rather than a silent universal truncation). A shorter-than-requested vector is a hard error — you can't safely pad your way to more information.
- **No task-type distinction.** Unlike a native provider SDK (e.g. Gemini's own API, which distinguishes `RETRIEVAL_QUERY` vs `RETRIEVAL_DOCUMENT` embeddings for better retrieval quality), the OpenAI wire format has no task-type field, so query and document embeddings go through this client symmetrically. If a project later adds a native, task-typed provider as a second `embed/` implementation, that's a different embedding space entirely — the `embedding_fingerprint`/`EmbeddingGuard` mechanism in §7/§8 is exactly what catches a corpus straddling both without a full re-embed.

Reranking is explicitly **deferred to v2**, not designed speculatively now. `xberg` has both a local cross-encoder and Cohere Rerank built in, so when it's needed it can plug into the sidecar without new infra — but the interface shape should be designed once there's a real v1 query/result shape to design it against, not guessed at up front.

### OCR: explicit opt-in, no self-hosted GPU

OCR is the one part of extraction that has a real, variable, per-page cost — whether that's local compute (Tesseract/Candle CPU inference) or a paid API call (a hosted OCR/vision endpoint). Left implicit, it's a silent cost multiplier: any scanned PDF or image-only upload triggers it automatically, and nobody decided that should happen. `valiro-go` doesn't currently gate this at all — it just lets xberg auto-OCR whatever needs it, with a comment explicitly calling that a deliberate choice *for that product* ("a hardcoded list here would refuse capability we paid for"). For a library used across multiple SaaS products with different cost sensitivities, that default doesn't generalize — this is a place `ragit` should be stricter than its reference implementation, not a place it copies it.

Decisions for `ragit`:
- **OCR is disabled by default**, both globally and per-document/per-tenant. A caller has to explicitly opt a document (or a whole tenant) in before any OCR spend happens — mirroring the `maxChunksPerDoc` cost guardrail in §7. Extraction of a scanned/image-only document with OCR disabled fails cleanly and legibly ("this document needs OCR, which is disabled for this tenant"), not silently, and not automatically-enabled.
- **No self-hosted GPU OCR/VLM infrastructure.** When OCR is enabled, the backend is one of two CPU-safe options, both reachable behind a small `OCREngine`-style interface so the app layer doesn't care which is active:
  1. **xberg's local, CPU-only backends** (Tesseract / Candle) — the cheap default when enabled, no external per-page billing, runs inside the xberg sidecar's own container so it inherits the same containment story as the rest of extraction (§6).
  2. **EdenAI's universal OCR API** — a paid, per-page, hosted alternative for cases where local OCR quality isn't good enough (handwriting, poor scans, non-Latin scripts) — the same gateway already used for embeddings above, so it's one fewer provider relationship to manage, not a new one.
- xberg's PaddleOCR (ONNX, can use GPU) and VLM-OCR (hosted vision-LLM calls via `liter-llm`) backends are **not** part of the default configuration — PaddleOCR because it implies GPU infra this design explicitly avoids running, VLM-OCR because it's effectively a third paid-per-page path that duplicates what routing to EdenAI already covers, without the benefit of using the same gateway relationship as embeddings.

This keeps OCR cost governance in the same place as embedding cost governance conceptually (§7's chunk cap, §8's fingerprint guard): a cheap thing must be explicitly turned on, never silently triggered by document content alone.

## 6. Resilience: the extractor is a fallback chain, not a single call

This is the single most concrete, incident-driven lesson in the reference implementation, and it's missing entirely from the original draft of this doc. Read this section with the adoption history above in mind: it describes a retrofit onto an existing local-parser stack, not a design built with xberg as a given from day one.

**The incident (2026-07-22, ~6.5 months into running the original local-parser stack)**: a 212 kB PDF drove a pure-Go PDF parser's array-reading path to ~5 GB of allocation and the kernel OOM-killed the whole app process — "the tenth such kill in 30 days." There was no per-parser memory ceiling, so the kernel picked a victim host-wide (the app every time, but Postgres was equally eligible). The same class of risk exists in `zip.NewReader` (.docx is a zip-bomb target), `excelize` (allocates whatever dimension range a file declares), and unbounded `csv.ReadAll`. xberg was still five days away from being adopted at all.

**The fix was two-layered, and both layers matter independently:**

1. **`IsolatedExtractor`** — runs local parsing in a short-lived *child process* (the same binary, re-invoked as a hidden CLI subcommand) with a hard memory cap (512 MB) and a timeout (60s). A blow-up kills the child; the parent marks that one document failed and keeps running. This layer exists so that *any* future parser pathology — including in a library not yet audited — is contained by construction, not by trusting each parser individually.
2. **`XbergExtractor`** — xberg runs as its own container with its own cgroup memory limit, so a blow-up there can't reach the Go process or Postgres at all. This is a stronger, cheaper version of the same containment idea, for the formats xberg handles.

**The fallback rule is deliberately narrow, and this is the part worth internalizing**: fallback (xberg → isolated local parser → in-process parser) fires *only* on a transport/deployment failure — connection refused, DNS failure, client timeout, spawn failure, or a 5xx. It does **not** fire when the service or the child *rejected the document itself* (a 400/422 from xberg — corrupt file, unsupported type; or a clean parser error from the isolated child). Retrying a bad file through progressively less-contained code paths is precisely the path that caused the OOM incident in the first place — "a bad document must never buy its way back in." A document-level rejection is a real, terminal failure, reported as one.

Recommended layering for `ragit`'s `extract/` package, in order of preference:
1. `XbergExtractor` (sidecar, when configured) — broadest format support, best containment.
2. `IsolatedExtractor` (capped child process) — fallback when xberg is unreachable, or the only path when xberg isn't configured at all.
3. Raw in-process parsers — used *inside* the isolated child, or as a last-resort fallback when even spawning a child process fails (e.g., in tests, or a stripped image with no CLI) — never given directly to code handling untrusted uploads.

**Building this fresh vs. inheriting it as history.** In `valiro-go`, layer 2 exists because layer 1 didn't exist yet when the OOM incident hit — it's a scar, not a spec. For `ragit`, starting with xberg available from day one, the calculus is different and worth deciding deliberately rather than defaulting to copying all three layers:
- If `ragit` treats xberg as effectively required (not optional-with-graceful-degradation), most of the OOM risk is already contained by xberg's own process/cgroup isolation for every format it supports, and layer 2's value shrinks to covering only the narrow slice of formats xberg doesn't handle — which may not be worth a child-process-spawning subsystem on its own.
- If `ragit` keeps xberg fully optional (§4's recommendation, since not every deploying project may want the sidecar dependency), then the local-parser path is a real, load-bearing fallback again — untrusted bytes reach `ledongthuc/pdf`/`excelize`/etc. directly whenever xberg is absent — and layer 2's containment is worth having from the start rather than waiting for your own version of the incident above.
This is why it's listed as an open question in §11 rather than settled here.

## 7. Job design: one resumable job, not a chain of three

The original draft recommended chaining `ExtractDocumentJob → ChunkDocumentJob → EmbedChunksBatchJob` via River continuations, reasoning that per-stage retry scoping was worth it. **The reference implementation deliberately does not do this, and ran into the specific failure mode that explains why not.**

**The incident**: embedding a large document is expensive. With chunk-then-embed split so that a failure meant "delete all chunks for this document, start over," a transient embedder timeout near the end of a long document — followed by River's automatic retry — would re-delete and re-embed *every* chunk from index 0 again. A document that timed out repeatedly near the end could be billed for its embeddings many times over and still never complete.

**The fix**: one `DocumentProcessWorker` job runs extract → chunk → embed → store for a document, but embedding is internally batched (10 chunks/batch) and **each batch is persisted to Postgres as soon as it succeeds**, tagged with an `embedding_fingerprint` (`provider|model|dimension`). On retry, the job reloads already-embedded chunks, and a chunk is treated as reusable only if its fingerprint matches the *currently active* embedder **and** its content still matches the freshly re-chunked text at that index — any mismatch (provider switch, re-chunked content, stray index) wipes and restarts clean rather than risk mixing embedding spaces within one document. This turns "retry" into "resume," not "redo."

This does give up independent per-stage concurrency limits (e.g., a separate `embed` queue rate-limited to the provider's API vs. an `extract` queue sized to OCR capacity) — if that separation genuinely matters at your volume, it's a legitimate reason to split jobs, but do it with the same resumable-checkpoint discipline, not naive delete-and-restart. For most `ragit`-using projects, one resumable job is the safer default; only split it under real evidence of a bottleneck.

**Error taxonomy** in the River worker (worth carrying over directly):
- **Permanent** (corrupt file, unsupported type — a document-level rejection per §6) → `river.JobCancel(err)`, no retry.
- **Rate-limited** by the embedding/OCR provider → `river.JobSnooze(backoff)`, a longer, deliberate wait rather than River's normal exponential backoff.
- **Transient** (anything else — network blip, momentary 5xx) → return the error, let River's normal retry/backoff handle it.

**Cost guardrail**: cap chunks per document (`maxChunksPerDoc`, 0 = unbounded). A pathological document (e.g. a 22k-chunk spreadsheet rendered as prose) that exceeds the cap is *not* embedded — it's flagged `skipped_too_large` and chunks are cleared, rather than silently consuming the embedding budget. An explicit override (superadmin reprocess) can force it through. This is a real, cheap guardrail worth including in `ragit` from the start rather than discovering the need for it after a large document blows a monthly embedding budget.

**Idempotency on extraction**: key on `(document_id, content_hash)` so re-uploads of identical bytes short-circuit before hitting xberg or an embedding API at all.

## 8. Data model (Postgres, via sqlc)

```sql
create table documents (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null,
    -- optional nested scope, if a project needs it — see "scope" note below
    scope_id        uuid,
    source_uri      text not null,        -- s3://bucket/tenant/doc/original.ext
    filename        text not null,
    mime_type       text not null,
    status          text not null,        -- pending|processing|ready|error|skipped_too_large
    error           text,
    text_content     text,                -- full extracted text, stored directly (see note)
    metadata        jsonb not null default '{}',  -- page count, language, tables, warnings from xberg
    chunk_count     int,
    embedding_model text,
    processed_at    timestamptz,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);

create table chunks (
    id                     uuid primary key default gen_random_uuid(),
    document_id            uuid not null references documents(id) on delete cascade,
    tenant_id              uuid not null,        -- denormalized for RLS / index filtering, see resync note
    scope_id               uuid,                 -- denormalized copy of documents.scope_id
    -- ephemeral attachment scopes (e.g. "this chunk belongs to one chat session"),
    -- separate from the durable-library scope above; see "ephemeral chunks" note
    session_id             uuid,
    chunk_index            int not null,
    heading_path           text[],               -- e.g. {"Chapter 2","Section 2.1"} for citations
    content                text not null,        -- markdown chunk
    embedding              vector(1536),         -- see "one active embedder" note below
    embedding_fingerprint  text,                 -- "provider|model|dimension", for resume + guard (§7)
    search_vector          tsvector generated always as (to_tsvector('english', content)) stored,
    metadata               jsonb not null default '{}',
    created_at             timestamptz not null default now()
);

create index chunks_tenant_idx on chunks (tenant_id);
create index chunks_embedding_hnsw on chunks using hnsw (embedding vector_cosine_ops);
create index chunks_search_vector_idx on chunks using gin (search_vector);
```

Notes, several of which revise the original draft's embedding-storage design:

- **One active embedder, not a multi-model table.** The original draft proposed a `chunk_embeddings(chunk_id, model_id)` table so different tenants could run different providers/dimensions simultaneously without a migration. The reference implementation does the opposite, deliberately: **one `vector` column, one active embedder platform-wide**, plus an `embedding_fingerprint` column per chunk and an **`EmbeddingGuard`** computed at startup — a query that counts chunks whose fingerprint doesn't match the live embedder. In `strict` mode a misalignment blocks RAG queries outright; otherwise it's fail-open and logged. Switching `EMBEDDING_PROVIDER` is then a loud, explicit event (re-embed the corpus) rather than a silent retrieval-quality footgun where old and new vectors get compared against each other in the same index. **This is the simpler, proven starting point — use it.** Reach for the multi-model table only if a real product requirement shows up for *simultaneous* multi-provider coexistence (e.g. true per-tenant BYO-key with different providers *without* a global re-embed event) — that's a real cost (wider index, join instead of a column) that isn't justified until something needs it.
- **`text_content` stored directly in Postgres**, not just as an S3 artifact. Simpler than the original draft's `s3://.../content.md` derived-artifact approach, and avoids a storage round-trip for reprocessing. Worth keeping S3 as the *original* file's home, but storing extracted text directly in the row unless documents get large enough that this becomes a real row-size concern.
- **Scope is richer than flat `tenant_id` in practice, and it's worth planning for even in a "generic" library.** The reference implementation has org → project → work-package as a nested hierarchy with cascading search (query at the project level and get project + work-package docs; query at the org level and always include org-level docs), plus a *separate* ephemeral attachment scope (documents/chunks tied to one conversation or one agent session, with a retention clock and a scheduled cleanup job — `DeleteExpiredConversationChunks`). Model this in `ragit` as: a required `tenant_id`, an optional nested `scope_id` (a project/workspace/whatever the host app calls it) for cascading search, and an optional `session_id` for ephemeral, auto-expiring attachments that are excluded from normal library search by default. Don't build the full cascade logic speculatively — but do reserve the columns, since retrofitting a scope hierarchy onto a flat `tenant_id` design later is a real migration, and this shape is now validated by a real product needing exactly it.
- **Denormalized scope columns need an explicit resync path.** A genuinely non-obvious gotcha from production: chunks snapshot `tenant_id`/`scope_id` at processing time for query-speed (avoiding a join on every search). If a document later moves scope (e.g. reassigned to a different project), nothing keeps the chunks in sync — and re-running the embedding job does *not* fix it, because the resume check in §7 sees identical content and skips rewriting those columns entirely. `ragit` needs an explicit `ResyncChunkScope` operation, called whenever a document's scope changes, not an assumption that reprocessing handles it.
- Row-level security keyed on `tenant_id` from day one, not deferred.
- `metadata jsonb` on `documents` is where xberg's structured output lands, queryable without a migration every time xberg adds a field.

## 9. Search

Two separate, unfused queries, not a single RRF-blended "hybrid search" endpoint — that's simpler than the original draft's plan and matches what's actually running:

- **Vector**: `ORDER BY embedding <=> query_embedding LIMIT top_k`, with a `min_score` cutoff (`1 - cosine_distance`). **The threshold is provider/model-specific and must be tuned empirically, not assumed** — e.g. Gemini embeddings typically score 0.5–0.7 for genuinely relevant matches, which is a very different range than OpenAI's. Don't hardcode a "good" cosine threshold in the library; make it configurable and document that it needs calibration per embedding model.
- **Full-text**: Postgres `tsvector` + `websearch_to_tsquery('simple', ...)` (the `'simple'` config — language-agnostic, no stemming — rather than `'english'`, worth matching unless there's a reason to want stemming), ranked by `ts_rank`.

Both queries carry the same scope-filtering predicate shape: `tenant_id` always, optional nested-scope filters, and a visibility/ACL boolean (`viewer_can_see_internal OR field_visible = TRUE`) — phrased so the *zero value is the restrictive one*, i.e. a caller that forgets to set it under-fetches rather than leaking. That phrasing detail is worth carrying directly into `ragit`'s search API: default to the narrowest visibility, require the caller to explicitly widen it, never the reverse.

Fusing vector + full-text server-side (RRF or similar) is a legitimate v2 enhancement, not something to build speculatively — the reference system runs both as separate callable queries and lets the caller decide, which is a reasonable place to start.

## 10. Proposed library shape (`ragit`)

```
ragit/
  extract/      // Extractor interface; xberg (sidecar) + isolated-child-process + raw-parser fallback chain (§6)
  ocr/          // OCREngine interface: xberg-local (Tesseract/Candle, CPU) + EdenAI universal OCR; off by default (§5)
  chunk/        // markdown-header-aware + recursive-character chunker, Go-owned (§5)
  embed/        // single OpenAI-wire-compatible client (default: EdenAI), fingerprint + alignment guard (§5, §8)
  store/        // S3 client wrapper (put/get, presigned URLs, tenant-prefixed keys)
  db/           // sqlc-generated queries + migrations (documents, chunks)
  jobs/         // River: one resumable DocumentProcessWorker (§7), DocumentDeleteWorker, retention-cleanup worker
  search/       // vector search + full-text search (separate queries, §9), scope-cascade support
  ragit.go      // façade: Ingest(ctx, tenant, file) / Search(ctx, tenant, query) / VectorSearch / FullTextSearch
```

Each SaaS project imports `ragit`, provides its own Postgres pool / S3 client / River client / config (xberg URL — optional, degrades gracefully per §4 — embedding base URL/key, OCR enabled + backend per tenant, chunk size, min-score threshold per model), and gets ingestion + retrieval as a small set of calls.

## 11. Decided vs. still open

Settled, now with production evidence behind most of it:
- **Embedding**: a single client speaking the OpenAI embeddings wire format, not per-provider adapters — default backend **EdenAI**'s EU gateway, matching what's already running in production. One active provider per deployment with a fingerprint + alignment guard rather than N-way coexistence (§5, §8).
- **Extraction, chunking, embedding are three separately-owned concerns**: xberg does extraction only; chunking and embedding both stay in Go (§5).
- **OCR is opt-in, never automatic, no self-hosted GPU.** Off by default; when enabled, backend is either xberg's local CPU OCR (Tesseract/Candle) or EdenAI's universal OCR API, behind one interface (§5).
- **Reranking is deferred to v2, deliberately not designed now** — the interface will be shaped by real v1 query/result data, not guessed at up front. xberg's local cross-encoder / Cohere Rerank remain the likely v2 backend, pluggable into the existing sidecar (§5).
- **One resumable job per document**, not a chain of per-stage jobs (§7).
- **Multi-tenancy**: `tenant_id` required everywhere, RLS from day one, plus reserved (not necessarily implemented) columns for a nested scope and an ephemeral session scope (§8).
- **Extraction resilience**: xberg → capped-child-process fallback → raw parser, fallback restricted to transport failures only, never document-level rejections (§6).

Settled during implementation (Phase 3):
- **Tables are prefixed `ragit_`** (`ragit_documents`, `ragit_chunks`, `ragit_migrations`), following River's `river_*` convention. This library is imported into a host application's database alongside the host's own tables and River's; unprefixed `documents`/`chunks` would collide, and there is no cheap moment to rename after the first deployment. A configurable Postgres schema — River's other namespacing lever — is deliberately *not* implemented: sqlc has no native schema templating, and the prefix already solves the collision problem.
- **`ragit` owns its migration line.** Migrations are embedded (`migrations/migrations.go`) and applied through `ragit.Migrate(ctx, pool)`, tracked in a `ragit_migrations` version table rather than goose's default `goose_db_version`. A host app runs its own migration tool over its own tables and upgrades `ragit` independently; neither has to know the other's version numbers. Shipping loose `.sql` files would have forced every consuming project to re-vendor and renumber SQL on every upgrade.
- **The scope columns are reserved but not cascading.** `scope_id` and `session_id` exist on both tables and are filterable, with `session_id` restrictive by default (an ephemeral attachment is invisible to library search unless a caller names its session). The nested org → project → work-package *cascade* is still not implemented — it needs a real product requirement to pin down its semantics. `MoveDocumentScope` exists and performs the `ResyncChunkScope` the denormalized copies require.
- **RLS is enforced, with one deployment caveat that matters.** Both tables carry `ENABLE` + `FORCE ROW LEVEL SECURITY` with a policy reading a `ragit.tenant_id` GUC, and every query runs inside a tenant-scoped transaction (`db.WithTenant`). The caveat, found the hard way while building this: **PostgreSQL exempts superusers and `BYPASSRLS` roles from row-level security entirely, `FORCE` or not.** The stock postgres Docker image's `POSTGRES_USER` is a superuser, so a test suite or deployment connecting as it sees every policy silently do nothing. The test harness now creates and connects as an unprivileged role specifically so the policies are exercised; a deployment must do the same or RLS is decorative.
- **`COPY FROM` is incompatible with RLS.** PostgreSQL rejects it outright (`COPY FROM not supported with row-level security`), so chunk inserts use a pgx batch (`:batchexec`) rather than sqlc's `:copyfrom`. At `embedBatchSize` rows per call this costs nothing — both are one round-trip.
- **The `search_vector` column uses the `'simple'` text-search config**, matching the `websearch_to_tsquery('simple', ...)` used at query time. An `'english'` column queried with a `'simple'` query stores stemmed lexemes and matches unstemmed terms against them, which under-matches silently rather than erroring.
- **Vector search filters on `embedding_fingerprint`.** Chunks embedded in a different embedding space are excluded rather than ranked — cosine distance across models is not a weaker signal but a meaningless one. `Searcher.CountMisalignedChunks` makes the straddled state detectable instead of silent; what to do about it (refuse to serve vs. serve degraded) is left to the host application.

Still open before implementation:
- Whether the nested-scope cascade (§8) is needed at launch for the first `ragit`-using project, or whether the reserved columns keep sitting unused.
- Self-hosted xberg sidecar vs. any managed xberg backend — self-hosted keeps data in your infra, which likely matters for data-residency requirements, and is the assumption elsewhere in this doc.
- Whether `ragit` treats xberg as effectively required or keeps it fully optional (§4) — this decides whether the isolated-child-process fallback layer (§6) is worth building on day one or only after xberg-is-absent local parsing proves to be a real, sustained code path rather than an edge case.
- Whether OCR opt-in is a tenant-level setting, a per-document flag, or both — production doesn't have this gate at all today, so there's no precedent to lean on for the granularity, only for the fact that *some* gate is needed.

## Recommendation

Start with **variation B** (xberg as an optional sidecar REST service, gated by config, degrading gracefully when absent) doing extraction only. Chunking and embedding both stay in Go — chunking because it's tightly coupled to citation metadata, embedding because provider choice is a per-tenant decision. Embedding goes through a **single OpenAI-wire-compatible client**, pointed at **EdenAI** by default, rather than bespoke per-provider adapters. **OCR is off by default and never triggers automatically**; when a tenant/document turns it on, it runs through either xberg's local CPU backend or EdenAI's OCR API, behind one small interface — no self-hosted GPU OCR infrastructure. **Reranking is deferred to v2.** Run the whole per-document pipeline as **one resumable River job** with per-batch checkpointing and an embedding-fingerprint resume check, not a chain of per-stage jobs — that chain looks cleaner on paper but a real system got burned re-billing itself for a partially-embedded document on every retry. Use **one active embedder platform-wide** with a fingerprint + alignment guard rather than a multi-model table, until something concrete needs true multi-provider coexistence. Tenant-scoped tables with RLS from day one, vector and full-text search as two separate callable queries rather than a fused hybrid endpoint until there's a real reason to fuse them.

This is the version that generalizes cleanly to "several SaaS projects": each project supplies config (DB pool, S3 bucket, xberg URL if any, embedding provider/key), and gets a pipeline whose hardest lessons — containment, resumable billing-safe retries, embedding-space alignment — are already paid for.
