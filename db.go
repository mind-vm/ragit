package ragit

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb"
)

// TenantGUC is the session variable the row-level security policies read to
// decide which rows are visible.
const TenantGUC = "ragit.tenant_id"

// MaintenanceGUC opts a transaction out of tenant scoping for reads and
// deletes. See [WithMaintenance]; it is set in exactly one place.
const MaintenanceGUC = "ragit.maintenance"

// NewPool opens a pool wired the way ragit needs.
//
// It exists because two pieces of setup are easy to omit and fail unhelpfully
// when they are. pgvector's binary codec needs the extension's OID, which only
// exists once the extension is installed, so it is registered per connection —
// without it embeddings still move, as text, several times slower. A pool built
// by hand works too; it just has to do this:
//
//	cfg.AfterConnect = sqlb.RegisterVectorType
//
// The role this connects as matters as much as the codec: PostgreSQL exempts
// superusers and BYPASSRLS roles from row-level security entirely, so a pool
// connected as one has ragit's tenant policies silently doing nothing. Connect
// as an ordinary role.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("ragit: parse dsn: %w", err)
	}
	cfg.AfterConnect = sqlb.RegisterVectorType
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("ragit: connect: %w", err)
	}
	return pool, nil
}

// WithTenant runs fn inside a transaction scoped to one tenant.
//
// The scoping is the GUC the row-level security policies read. That is a
// second layer beneath the confinement predicates ragit's own queries carry:
// the predicates constrain what ragit asks for, and RLS constrains what the
// database will answer regardless of who asks — a raw pgx call, a psql
// session, a query written later by code that never heard of [Scope].
//
// With FORCE ROW LEVEL SECURITY enabled, a query run outside such a
// transaction sees zero rows rather than every row: the policies fail closed.
//
// The caveat worth knowing at deployment time: PostgreSQL exempts superusers
// (and BYPASSRLS roles) from row-level security, FORCE or not. The stock
// postgres image's POSTGRES_USER is a superuser, so an application connecting
// as one has these policies silently inert and is relying on the predicates
// alone. See [NewPool].
func WithTenant(ctx context.Context, pool *pgxpool.Pool, tenantID string, fn func(sqlb.Executor) error) error {
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", TenantGUC, tenantID); err != nil {
			return fmt.Errorf("ragit: set tenant scope: %w", err)
		}
		return fn(tx)
	})
}

// WithMaintenance runs fn in a transaction that can read and delete across
// every tenant.
//
// This exists for one caller — the retention sweep — and the reason it needs
// an escape at all is that the work is inherently cross-tenant: finding
// expired rows means reading rows whose owning tenants cannot be enumerated
// beforehand, and enumerating them would itself be the cross-tenant read.
//
// It widens reads and deletes only. The policies' WITH CHECK clause stays
// tenant-scoped, so nothing reached from here can write a row into, or move a
// row between, tenants.
//
// Do not reach for this to make an ordinary query simpler. Every use is a
// place where isolation rests on the surrounding code being correct rather
// than on the database, which is what [WithTenant] exists to avoid.
func WithMaintenance(ctx context.Context, pool *pgxpool.Pool, fn func(sqlb.Executor) error) error {
	return inTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, 'on', true)", MaintenanceGUC); err != nil {
			return fmt.Errorf("ragit: enter maintenance scope: %w", err)
		}
		return fn(tx)
	})
}

func inTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ragit: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ragit: commit transaction: %w", err)
	}
	return nil
}
