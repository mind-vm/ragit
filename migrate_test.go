package ragit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/internal/testutil"
	"github.com/jryannel/ragit/ragitschema"
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
	require.True(t, columnExists(t, pool, "ragit_chunks", "search_vector"))
	require.True(t, columnExists(t, pool, "ragit_documents", "scope_a_id"))
	require.True(t, columnExists(t, pool, "ragit_documents", "scope_b_id"))

	// The version table is ragit's own, not goose's default — this is what
	// keeps a host app's migration sequence from colliding with this one.
	require.True(t, tableExists(t, pool, "ragit_migrations"))
	require.False(t, tableExists(t, pool, "goose_db_version"))

	// Safe to call on every startup.
	require.NoError(t, ragit.Migrate(ctx, pool))
}

// The Down direction of the hand-written migration drops the generated
// tsvector column and restores the RLS policies. Nothing else exercises that
// SQL, and a broken rollback is only ever discovered at the worst moment.
func TestMigrate_HandwrittenMigrationRoundTrips(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool))
	require.True(t, columnExists(t, pool, "ragit_chunks", "search_vector"))
	require.True(t, policyMentionsMaintenance(t, pool, "ragit_documents"))

	require.NoError(t, ragit.MigrateDown(ctx, pool))
	require.False(t, columnExists(t, pool, "ragit_chunks", "search_vector"))
	require.False(t, policyMentionsMaintenance(t, pool, "ragit_documents"),
		"rolling back must remove the policies, not just the column")
	require.False(t, policyMentionsMaintenance(t, pool, "ragit_chunks"))

	// The tables themselves survive a single-step rollback.
	require.True(t, tableExists(t, pool, "ragit_documents"))

	// And it comes back cleanly.
	require.NoError(t, ragit.Migrate(ctx, pool))
	require.True(t, columnExists(t, pool, "ragit_chunks", "search_vector"))
	require.True(t, policyMentionsMaintenance(t, pool, "ragit_documents"))
}

// The embedding dimension is a value in the schema declaration rather than a
// literal in a shipped .sql file, so a deployment that needs a different width
// renders its own migration set instead of forking one.
func TestMigrate_EmbeddingDimensionIsDeclared(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()
	require.NoError(t, ragit.Migrate(ctx, pool))

	var typmod int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT atttypmod FROM pg_attribute
		WHERE attrelid = 'ragit_chunks'::regclass AND attname = 'embedding'`).Scan(&typmod))
	require.Equal(t, ragitschema.DefaultEmbeddingDimension, typmod,
		"the shipped migrations declare the default width")

	// The declaration itself takes the dimension as an argument.
	require.Equal(t, 768, ragitschema.New(768).Dimension)
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

// policyMentionsMaintenance reports whether the table's RLS policy carries the
// maintenance escape. A rolled-back schema has no policy row at all, which is
// a "no" rather than an error — the whole point of calling this after a
// MigrateDown.
func policyMentionsMaintenance(t *testing.T, pool *pgxpool.Pool, table string) bool {
	t.Helper()
	var qual string
	err := pool.QueryRow(context.Background(),
		"SELECT coalesce(qual, '') FROM pg_policies WHERE tablename = $1", table,
	).Scan(&qual)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return strings.Contains(qual, "ragit.maintenance")
}
