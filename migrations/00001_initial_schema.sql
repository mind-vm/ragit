-- +goose Up
-- +goose StatementBegin
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pgcrypto;
-- +goose StatementEnd

-- Tables are prefixed ragit_ because this library is imported into a host
-- application's own database, alongside the host's tables and River's
-- river_* tables. Unprefixed "documents"/"chunks" would collide with a host
-- app's own schema sooner or later. Same convention River uses.

-- +goose StatementBegin
CREATE TABLE ragit_documents (
    id              uuid primary key default gen_random_uuid(),
    tenant_id       uuid not null,
    -- Reserved for a nested scope hierarchy (project/workspace/whatever the
    -- host app calls it). Columns exist from day one because retrofitting a
    -- hierarchy onto a flat tenant_id is a real migration; the cascading
    -- lookup itself is deliberately NOT implemented yet. See design.md §8.
    scope_id        uuid,
    -- Reserved for ephemeral attachments tied to one conversation/agent
    -- session, excluded from normal library search by default.
    session_id      uuid,
    source_uri      text,
    filename        text not null,
    mime_type       text not null,
    status          text not null default 'pending', -- pending|processing|ready|error|skipped_too_large
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
CREATE INDEX idx_ragit_documents_tenant_id ON ragit_documents(tenant_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_documents_scope ON ragit_documents(tenant_id, scope_id) WHERE scope_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE ragit_chunks (
    id                     uuid primary key default gen_random_uuid(),
    document_id            uuid not null references ragit_documents(id) on delete cascade,
    -- tenant_id/scope_id/session_id are denormalized copies of the parent
    -- document's, snapshotted at processing time so search never needs a
    -- join to filter. That snapshot does not self-heal: moving a document
    -- between scopes requires an explicit resync, because the resume check
    -- in design.md §7 sees identical content and skips rewriting the row.
    tenant_id              uuid not null,
    scope_id               uuid,
    session_id             uuid,
    chunk_index            int not null,
    heading_path           text[],
    content                text not null,
    embedding              vector(1536),
    embedding_fingerprint  text,
    -- 'simple' rather than 'english': language-agnostic, no stemming, and it
    -- must match the websearch_to_tsquery() config used at query time in
    -- search/ — a stemmed vector queried with unstemmed terms silently
    -- under-matches. See design.md §9.
    search_vector          tsvector generated always as (to_tsvector('simple', content)) stored,
    metadata               jsonb not null default '{}',
    created_at             timestamptz not null default now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_tenant_id ON ragit_chunks(tenant_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_document_id ON ragit_chunks(document_id);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_search_vector ON ragit_chunks USING gin(search_vector);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_fingerprint ON ragit_chunks(tenant_id, embedding_fingerprint);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_scope ON ragit_chunks(tenant_id, scope_id) WHERE scope_id IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_session ON ragit_chunks(session_id) WHERE session_id IS NOT NULL;
-- +goose StatementEnd

-- Row-level security from day one (design.md §8). Every ragit query runs
-- inside a transaction that sets ragit.tenant_id (see internal/db/tenant.go),
-- so a query that forgets its tenant predicate returns zero rows rather than
-- another tenant's. FORCE is required: without it the table owner — which is
-- what most applications connect as — bypasses the policy entirely.
--
-- NULLIF guards the empty string: current_setting(..., true) yields NULL when
-- the GUC was never set, but '' when it was set and reset, and ''::uuid errors.
-- Either way the policy evaluates to NULL, which is not true, so it fails closed.

-- +goose StatementBegin
ALTER TABLE ragit_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ragit_documents FORCE ROW LEVEL SECURITY;
CREATE POLICY ragit_documents_tenant_isolation ON ragit_documents
    USING (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE ragit_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE ragit_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY ragit_chunks_tenant_isolation ON ragit_chunks
    USING (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS ragit_chunks;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE IF EXISTS ragit_documents;
-- +goose StatementEnd
