package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantGUC is the session variable the row-level security policies in
// migration 00001 read to decide which rows are visible.
const TenantGUC = "ragit.tenant_id"

// MaintenanceGUC opts a transaction out of tenant scoping for reads and
// deletes. See [WithMaintenance]; it is set in exactly one place.
const MaintenanceGUC = "ragit.maintenance"

// WithTenant runs fn inside a transaction that has the tenant GUC set, so
// the RLS policies on ragit_documents/ragit_chunks resolve to that tenant.
//
// This is not merely a convenience: with FORCE ROW LEVEL SECURITY enabled,
// a query run outside such a transaction sees zero rows rather than every
// row — the policies fail closed. Every ragit database access goes through
// here.
//
// One caveat worth knowing when deploying: PostgreSQL exempts superusers
// (and roles with BYPASSRLS) from row-level security entirely, FORCE or not.
// If the application connects as a superuser — which the stock postgres
// Docker image's POSTGRES_USER is — these policies are silently inert and
// tenant isolation rests on the query predicates alone. Connect as an
// ordinary role.
func WithTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(*Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ragit: begin tenant transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := setTenant(ctx, tx, tenantID); err != nil {
		return err
	}
	if err := fn(New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ragit: commit tenant transaction: %w", err)
	}
	return nil
}

// setTenant applies the GUC for the life of the transaction. set_config with
// is_local=true is used rather than SET LOCAL because SET does not accept a
// bind parameter, and interpolating a tenant id into DDL-ish SQL is exactly
// the kind of thing worth not doing.
func setTenant(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", TenantGUC, tenantID.String()); err != nil {
		return fmt.Errorf("ragit: set tenant scope: %w", err)
	}
	return nil
}

// WithMaintenance runs fn in a transaction that can read and delete across
// every tenant.
//
// This exists for one caller — the retention sweep — and the reason it needs
// an escape at all is that the work is inherently cross-tenant: finding the
// expired rows means reading rows whose owning tenants cannot be enumerated
// beforehand, and enumerating them would itself be the cross-tenant read.
//
// It widens reads and deletes only. Migration 00003 leaves the policies'
// WITH CHECK clause tenant-scoped, so nothing reached from here can write a
// row into, or move a row between, tenants.
//
// Do not reach for this to make an ordinary query simpler. Every use is a
// place where tenant isolation rests on the surrounding code being correct
// rather than on the database, which is precisely what WithTenant exists to
// avoid.
func WithMaintenance(ctx context.Context, pool *pgxpool.Pool, fn func(*Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("ragit: begin maintenance transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config($1, 'on', true)", MaintenanceGUC); err != nil {
		return fmt.Errorf("ragit: enter maintenance scope: %w", err)
	}
	if err := fn(New(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("ragit: commit maintenance transaction: %w", err)
	}
	return nil
}
