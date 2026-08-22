package ragit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/internal/testutil"
)

// Migrations run against a scratch database rather than the shared one, so
// rolling back cannot pull columns out from under other tests.

func TestMigrate_AppliesToEmptyDatabaseAndIsIdempotent(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool))

	require.True(t, tableExists(t, pool, "ragit_documents"))
	require.True(t, tableExists(t, pool, "ragit_chunks"))
	require.True(t, columnExists(t, pool, "ragit_chunks", "expires_at"))

	// The version table is ragit's own, not goose's default — this is what
	// keeps a host app's migration sequence from colliding with this one.
	require.True(t, tableExists(t, pool, "ragit_migrations"))
	require.False(t, tableExists(t, pool, "goose_db_version"))

	// Safe to call on every startup.
	require.NoError(t, ragit.Migrate(ctx, pool))
}

// The Down direction of 00003 restores the RLS policies to their pre-
// maintenance form. Nothing else exercises that SQL, and a broken rollback is
// only ever discovered at the worst possible moment.
func TestMigrate_RetentionMigrationRoundTrips(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool))
	require.True(t, columnExists(t, pool, "ragit_documents", "expires_at"))
	require.True(t, policyMentionsMaintenance(t, pool, "ragit_documents"))

	require.NoError(t, ragit.MigrateDown(ctx, pool))
	require.False(t, columnExists(t, pool, "ragit_documents", "expires_at"))
	require.False(t, columnExists(t, pool, "ragit_chunks", "expires_at"))
	require.False(t, policyMentionsMaintenance(t, pool, "ragit_documents"),
		"rolling back must remove the maintenance escape, not just the columns")
	require.False(t, policyMentionsMaintenance(t, pool, "ragit_chunks"))

	// The tables themselves survive a single-step rollback.
	require.True(t, tableExists(t, pool, "ragit_documents"))

	// And it comes back cleanly.
	require.NoError(t, ragit.Migrate(ctx, pool))
	require.True(t, columnExists(t, pool, "ragit_documents", "expires_at"))
	require.True(t, policyMentionsMaintenance(t, pool, "ragit_documents"))
}

func TestMigrate_EnablesForcedRowLevelSecurity(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()
	require.NoError(t, ragit.Migrate(ctx, pool))

	for _, table := range []string{"ragit_documents", "ragit_chunks"} {
		var enabled, forced bool
		require.NoError(t, pool.QueryRow(ctx,
			"SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = $1", table,
		).Scan(&enabled, &forced))
		require.True(t, enabled, "%s must have RLS enabled", table)
		require.True(t, forced, "%s must FORCE RLS, or the table owner bypasses every policy", table)
	}
}

func tableExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = $1)", name,
	).Scan(&exists))
	return exists
}

func columnExists(t *testing.T, pool *pgxpool.Pool, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2)",
		table, column,
	).Scan(&exists))
	return exists
}

func policyMentionsMaintenance(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var qual string
	require.NoError(t, pool.QueryRow(context.Background(),
		"SELECT coalesce(qual, '') FROM pg_policies WHERE tablename = $1", table,
	).Scan(&qual))
	return strings.Contains(qual, "ragit.maintenance")
}
