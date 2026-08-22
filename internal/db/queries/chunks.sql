-- name: CreateChunks :batchexec
-- Deliberately :batchexec and not :copyfrom. PostgreSQL rejects COPY FROM
-- outright on a table with row-level security enabled ("COPY FROM not
-- supported with row-level security"), which ragit_chunks has. A pgx batch
-- is one round-trip anyway, and batches here are embedBatchSize-sized.
INSERT INTO ragit_chunks (
  document_id, tenant_id, scope_id, session_id, chunk_index, heading_path,
  content, embedding, embedding_fingerprint, metadata
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: GetChunksByDocumentID :many
SELECT * FROM ragit_chunks WHERE document_id = $1 AND tenant_id = $2 ORDER BY chunk_index ASC;

-- name: GetChunkDigestsByDocumentID :many
-- Returns already-embedded chunks for a document so ProcessDocument can
-- resume an interrupted embedding run instead of re-embedding from scratch.
SELECT chunk_index, content, embedding_fingerprint
FROM ragit_chunks
WHERE document_id = $1 AND tenant_id = $2 AND embedding IS NOT NULL
ORDER BY chunk_index ASC;

-- name: ClearDocumentChunks :exec
DELETE FROM ragit_chunks WHERE document_id = $1 AND tenant_id = $2;

-- name: ResyncChunkScope :exec
-- Re-stamps the denormalized scope columns on a document's chunks. Required
-- whenever a document moves scope: reprocessing does NOT fix this, because
-- the resume check sees identical content and skips rewriting the rows
-- entirely. See design.md §8.
UPDATE ragit_chunks
SET scope_id = $3, session_id = $4
WHERE document_id = $1 AND tenant_id = $2;

-- name: CountChunksWithForeignFingerprint :one
-- Backs the embedding alignment guard: how many chunks were embedded in a
-- different embedding space than the currently-active embedder. Non-zero
-- means vectors that are not comparable are sharing one index.
SELECT count(*) FROM ragit_chunks
WHERE tenant_id = $1
  AND embedding IS NOT NULL
  AND (embedding_fingerprint IS DISTINCT FROM sqlc.arg('embedding_fingerprint')::text);

-- name: SearchChunksByVector :many
-- Vector search. The embedding_fingerprint predicate is not an optimization:
-- vectors produced by different providers/models occupy different spaces and
-- their cosine distances are meaningless against each other, so chunks
-- outside the active embedding space must never be ranked alongside those
-- inside it. min_score is a caller-supplied cutoff on cosine similarity and
-- is intentionally not defaulted to a "good" value — the useful range is
-- model-specific and has to be calibrated per embedder (design.md §9).
SELECT
  c.id,
  c.document_id,
  c.chunk_index,
  c.heading_path,
  c.content,
  c.metadata,
  d.filename,
  (1 - (c.embedding <=> sqlc.arg('query_embedding')::vector))::float8 AS score
FROM ragit_chunks c
JOIN ragit_documents d ON d.id = c.document_id
WHERE c.tenant_id = sqlc.arg('tenant_id')
  AND c.embedding IS NOT NULL
  AND c.embedding_fingerprint = sqlc.arg('embedding_fingerprint')::text
  AND (sqlc.narg('scope_id')::uuid IS NULL OR c.scope_id = sqlc.narg('scope_id')::uuid)
  AND (c.session_id IS NULL OR c.session_id = sqlc.narg('session_id')::uuid)
  AND (1 - (c.embedding <=> sqlc.arg('query_embedding')::vector)) >= sqlc.arg('min_score')::float8
ORDER BY c.embedding <=> sqlc.arg('query_embedding')::vector
LIMIT sqlc.arg('result_limit')::int;

-- name: SearchChunksByText :many
-- Full-text search, kept as a separate callable query rather than fused with
-- vector search — fusion (RRF or similar) is a v2 decision the caller can
-- make for itself today. The 'simple' config must match the one used by the
-- search_vector generated column in migration 00001.
SELECT
  c.id,
  c.document_id,
  c.chunk_index,
  c.heading_path,
  c.content,
  c.metadata,
  d.filename,
  ts_rank(c.search_vector, websearch_to_tsquery('simple', sqlc.arg('query')::text))::float8 AS score
FROM ragit_chunks c
JOIN ragit_documents d ON d.id = c.document_id
WHERE c.tenant_id = sqlc.arg('tenant_id')
  AND c.search_vector @@ websearch_to_tsquery('simple', sqlc.arg('query')::text)
  AND (sqlc.narg('scope_id')::uuid IS NULL OR c.scope_id = sqlc.narg('scope_id')::uuid)
  AND (c.session_id IS NULL OR c.session_id = sqlc.narg('session_id')::uuid)
ORDER BY score DESC, c.document_id, c.chunk_index ASC
LIMIT sqlc.arg('result_limit')::int;
