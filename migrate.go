package ragit

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mind-vm/ragit/internal/migrate"
)

// Migrate brings ragit's own tables up to the schema this build expects.
//
// ragit owns its migration line rather than shipping loose .sql files for a
// host application to vendor into its own sequence: the migrations are
// embedded in the binary and tracked in a ragit_migrations version table, so
// upgrading the library upgrades its schema, and a host app's own migration
// tool never has to know these tables exist. This mirrors how River manages
// its river_* tables.
//
// It is safe to call on every startup, and touches nothing outside the
// ragit_-prefixed tables.
//
// The connecting role must not be a superuser if the row-level security
// policies are to have any effect — PostgreSQL exempts superusers from RLS
// entirely, FORCE or not. Migrating as an admin role and then running the
// application as an ordinary one is the intended split.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate.Up(ctx, pool)
}

// MigrateDown rolls back the most recent migration. Intended for development
// and tests; rolling back a populated vector index rarely is what you want in
// production.
func MigrateDown(ctx context.Context, pool *pgxpool.Pool) error {
	return migrate.Down(ctx, pool)
}
