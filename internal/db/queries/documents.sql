-- name: CreateDocument :one
INSERT INTO documents (
  tenant_id, source_uri, filename, mime_type
) VALUES (
  $1, $2, $3, $4
) RETURNING *;

-- name: GetDocumentByID :one
SELECT * FROM documents WHERE id = $1 AND tenant_id = $2;

-- name: UpdateDocumentProcessing :exec
UPDATE documents SET status = 'processing', updated_at = now() WHERE id = $1;

-- name: UpdateDocumentReady :exec
UPDATE documents SET
  status = 'ready',
  text_content = $2,
  metadata = $3,
  chunk_count = $4,
  embedding_model = $5,
  processed_at = $6,
  updated_at = now()
WHERE id = $1;

-- name: UpdateDocumentError :exec
UPDATE documents SET
  status = 'error',
  error = $2,
  updated_at = now()
WHERE id = $1;

-- name: UpdateDocumentSkippedTooLarge :exec
UPDATE documents SET
  status = 'skipped_too_large',
  error = $2,
  chunk_count = 0,
  updated_at = now()
WHERE id = $1;

-- name: DeleteDocument :exec
-- Cascades to chunks via the FK in migration 00001.
DELETE FROM documents WHERE id = $1 AND tenant_id = $2;
