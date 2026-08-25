-- +goose Up
ALTER TABLE ragit_chunks ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;
CREATE INDEX idx_ragit_chunks_search_vector ON ragit_chunks USING gin(search_vector);
-- +goose StatementBegin
ALTER TABLE ragit_documents ENABLE ROW LEVEL SECURITY;
ALTER TABLE ragit_documents FORCE ROW LEVEL SECURITY;
CREATE POLICY ragit_documents_tenant_isolation ON ragit_documents
    USING (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
        OR current_setting('ragit.maintenance', true) = 'on'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
    );
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE ragit_chunks ENABLE ROW LEVEL SECURITY;
ALTER TABLE ragit_chunks FORCE ROW LEVEL SECURITY;
CREATE POLICY ragit_chunks_tenant_isolation ON ragit_chunks
    USING (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
        OR current_setting('ragit.maintenance', true) = 'on'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS ragit_chunks_tenant_isolation ON ragit_chunks;
ALTER TABLE ragit_chunks NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ragit_chunks DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
-- +goose StatementBegin
DROP POLICY IF EXISTS ragit_documents_tenant_isolation ON ragit_documents;
ALTER TABLE ragit_documents NO FORCE ROW LEVEL SECURITY;
ALTER TABLE ragit_documents DISABLE ROW LEVEL SECURITY;
-- +goose StatementEnd
DROP INDEX IF EXISTS idx_ragit_chunks_search_vector;
ALTER TABLE ragit_chunks DROP COLUMN search_vector;
