-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE documents (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null,
    source_uri      text,
    filename        text not null,
    mime_type       text not null,
    status          text not null default 'pending', -- pending|processing|ready|error
    error           text,
    text_content    text,
    metadata        jsonb not null default '{}',
    chunk_count     int,
    embedding_model text,
    processed_at    timestamptz,
    created_at      timestamptz not null default now(),
    updated_at      timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_documents_tenant_id ON documents(tenant_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE chunks (
    id                     uuid primary key default gen_random_uuid(),
    document_id            uuid not null references documents(id) on delete cascade,
    tenant_id              uuid not null,
    chunk_index            int not null,
    heading_path           text[],
    content                text not null,
    embedding              vector(1536),
    embedding_fingerprint  text,
    search_vector          tsvector generated always as (to_tsvector('english', content)) stored,
    metadata               jsonb not null default '{}',
    created_at             timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_chunks_tenant_id ON chunks(tenant_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_chunks_document_id ON chunks(document_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_chunks_search_vector ON chunks USING gin(search_vector);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS chunks;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS documents;
-- +goose StatementEnd
