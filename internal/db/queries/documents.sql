-- name: CreateDocument :one
INSERT INTO ragit_documents (
  tenant_id, scope_id, session_id, source_uri, filename, mime_type, expires_at
) VALUES (
  $1, $2, $3, $4, $5, $6, $7
) RETURNING *;

-- name: GetDocumentByID :one
SELECT * FROM ragit_documents WHERE id = $1 AND tenant_id = $2;

-- The Update* statements below filter on id alone. That is safe because
-- every one of them runs inside a WithTenant transaction, where the
-- ragit_documents RLS policy adds the tenant predicate itself; an id
-- belonging to another tenant matches zero rows.

-- name: UpdateDocumentProcessing :exec
UPDATE ragit_documents SET status = 'processing', updated_at = now() WHERE id = $1;

-- name: UpdateDocumentReady :exec
UPDATE ragit_documents SET
  status = 'ready',
  text_content = $2,
  metadata = $3,
  chunk_count = $4,
  embedding_model = $5,
  processed_at = $6,
  updated_at = now()
WHERE id = $1;

-- name: UpdateDocumentError :exec
UPDATE ragit_documents SET
  status = 'error',
  error = $2,
  updated_at = now()
WHERE id = $1;

-- name: UpdateDocumentSkippedTooLarge :exec
UPDATE ragit_documents SET
  status = 'skipped_too_large',
  error = $2,
  chunk_count = 0,
  updated_at = now()
WHERE id = $1;

-- name: UpdateDocumentScope :exec
-- Moves a document between scopes. Callers must follow this with
-- ResyncChunkScope — the chunks' denormalized copies do not self-heal.
UPDATE ragit_documents SET
  scope_id = $2,
  session_id = $3,
  updated_at = now()
WHERE id = $1;

-- name: DeleteDocument :exec
-- Cascades to ragit_chunks via the FK in migration 00001.
DELETE FROM ragit_documents WHERE id = $1 AND tenant_id = $2;

-- name: ListExpiredDocuments :many
-- Cross-tenant by design: the retention sweep cannot enumerate the tenants
-- that own expired rows without first reading across tenants. Runs under
-- db.WithMaintenance, which is the only place the maintenance GUC is set.
SELECT id, tenant_id, source_uri
FROM ragit_documents
WHERE expires_at IS NOT NULL AND expires_at <= now()
ORDER BY expires_at ASC
LIMIT sqlc.arg('result_limit')::int;

-- name: DeleteDocumentsByIDs :execrows
-- Cascades to ragit_chunks via the FK in migration 00001.
DELETE FROM ragit_documents WHERE id = ANY(sqlc.arg('ids')::uuid[]);

-- name: DeleteExpiredChunks :execrows
-- Chunks can outlive their document's retention clock when only the chunks
-- were given one (an attachment whose source file is still wanted). Runs
-- after the document sweep so it only sees genuinely orphaned expiries.
DELETE FROM ragit_chunks WHERE expires_at IS NOT NULL AND expires_at <= now();
