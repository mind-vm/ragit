-- name: CreateChunks :copyfrom
INSERT INTO chunks (
  document_id, tenant_id, chunk_index, heading_path, content, embedding, embedding_fingerprint, metadata
) VALUES (
  $1, $2, $3, $4, $5, $6, $7, $8
);

-- name: GetChunksByDocumentID :many
SELECT * FROM chunks WHERE document_id = $1 AND tenant_id = $2 ORDER BY chunk_index ASC;
