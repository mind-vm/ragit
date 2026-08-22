-- +goose Up
-- Kept separate from the initial schema: building an HNSW index is the one
-- slow step here, and on an existing corpus it is worth running (or
-- rebuilding CONCURRENTLY) independently of the table definitions.

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_embedding_hnsw ON ragit_chunks
    USING hnsw (embedding vector_cosine_ops) WITH (m = 16, ef_construction = 64);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_ragit_chunks_embedding_hnsw;
-- +goose StatementEnd
