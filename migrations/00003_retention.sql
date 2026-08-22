-- +goose Up

-- Ephemeral attachments (design.md §8) need a retention clock. expires_at is
-- nullable and NULL means "keeps forever", so every existing durable-library
-- row is unaffected by this migration.

-- +goose StatementBegin
ALTER TABLE ragit_documents ADD COLUMN expires_at timestamptz;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE ragit_chunks ADD COLUMN expires_at timestamptz;
-- +goose StatementEnd

-- Partial indexes: the sweep only ever asks about rows that have a clock at
-- all, which in a healthy corpus is a small minority of them.

-- +goose StatementBegin
CREATE INDEX idx_ragit_documents_expires_at ON ragit_documents(expires_at) WHERE expires_at IS NOT NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_ragit_chunks_expires_at ON ragit_chunks(expires_at) WHERE expires_at IS NOT NULL;
-- +goose StatementEnd

-- The retention sweep is inherently cross-tenant: it has to find expired rows
-- belonging to tenants it cannot enumerate in advance, and enumerating them
-- would itself be a cross-tenant read. So the policy gains a second, explicit
-- escape — a maintenance GUC — rather than the sweep running as a BYPASSRLS
-- role, which would exempt it from every policy rather than this one.
--
-- This does not weaken the security model: anything able to set
-- ragit.maintenance can already set ragit.tenant_id to any value it likes, so
-- the boundary was always "the application controls the GUCs", not "the
-- database distrusts the application". What it buys is that the escape is
-- visible in the schema and greppable in the code (db.WithMaintenance), not
-- an ambient property of a connection role.

-- +goose StatementBegin
ALTER POLICY ragit_documents_tenant_isolation ON ragit_documents
    USING (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
        OR current_setting('ragit.maintenance', true) = 'on'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
    );
-- +goose StatementEnd

-- +goose StatementBegin
ALTER POLICY ragit_chunks_tenant_isolation ON ragit_chunks
    USING (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
        OR current_setting('ragit.maintenance', true) = 'on'
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid
    );
-- +goose StatementEnd

-- Note the asymmetry above, which is deliberate: maintenance widens what can
-- be READ and DELETED, but WITH CHECK is left tenant-scoped, so no
-- maintenance path can write or move a row into a tenant it is not scoped to.

-- +goose Down

-- +goose StatementBegin
ALTER POLICY ragit_documents_tenant_isolation ON ragit_documents
    USING (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER POLICY ragit_chunks_tenant_isolation ON ragit_chunks
    USING (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_ragit_chunks_expires_at;
-- +goose StatementEnd

-- +goose StatementBegin
DROP INDEX IF EXISTS idx_ragit_documents_expires_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE ragit_chunks DROP COLUMN expires_at;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE ragit_documents DROP COLUMN expires_at;
-- +goose StatementEnd
