package ragit_test

import (
	"context"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/internal/testutil"
)

// renderedSet stands in for the output of `ragit-gen -dim N`: goose-formatted
// SQL declaring the chunk vector at a chosen width. Only the width and the
// version table matter to what is under test here, so this is the smallest set
// that carries both.
func renderedSet(dim string) fstest.MapFS {
	return fstest.MapFS{
		"00001_initial_schema.sql": &fstest.MapFile{Data: []byte(`
-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;
CREATE TABLE ragit_chunks (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    embedding vector(` + dim + `)
);

-- +goose Down
DROP TABLE ragit_chunks;
`)},
	}
}

// TestMigrateFromFS_UsesRagitsRunnerAndVersionTable is shape question 4: a
// consumer at another embedding dimension used to re-implement the runner,
// including remembering the version table's name. Getting that wrong is
// silent — goose starts a second history in its own default table and
// re-applies migrations the database already has.
func TestMigrateFromFS_UsesRagitsRunnerAndVersionTable(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool, ragit.FromFS(renderedSet("768"))))

	require.True(t, tableExists(t, pool, "ragit_chunks"))
	require.Equal(t, 768, storedDimension(t, pool))

	require.True(t, tableExists(t, pool, ragit.MigrationsTable))
	require.False(t, tableExists(t, pool, "goose_db_version"),
		"a rendered set must join ragit's migration line, not start goose's own")

	// Safe on every startup, exactly like the embedded set.
	require.NoError(t, ragit.Migrate(ctx, pool, ragit.FromFS(renderedSet("768"))))

	require.NoError(t, ragit.MigrateDown(ctx, pool, ragit.FromFS(renderedSet("768"))))
	require.False(t, tableExists(t, pool, "ragit_chunks"))
}

// TestMigrateFromFS_RefusesADimensionTheDatabaseCannotHold covers the failure
// that has no symptom: every rendered set carries the same version numbers, so
// applying a 768 set to a 1536 database finds those versions already applied
// and reports success having changed nothing at all.
func TestMigrateFromFS_RefusesADimensionTheDatabaseCannotHold(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool))
	before := storedDimension(t, pool)

	err := ragit.Migrate(ctx, pool, ragit.FromFS(renderedSet("768")))
	require.Error(t, err, "silently doing nothing is the failure this prevents")
	require.ErrorContains(t, err, "declare vector(768)")
	require.ErrorContains(t, err, "ragit_chunks.embedding is vector(1536)")

	require.Equal(t, before, storedDimension(t, pool), "a refused migration changes nothing")
	require.True(t, columnExists(t, pool, "ragit_chunks", "search_vector"),
		"the existing schema is left intact")
}

// TestMigrate_EmbeddedSetIsRefusedOnAForeignDimension is the same guard in the
// direction a consumer hits by accident: a host application that renders its
// own schema, then calls plain ragit.Migrate somewhere in its startup path.
func TestMigrate_EmbeddedSetIsRefusedOnAForeignDimension(t *testing.T) {
	pool := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	require.NoError(t, ragit.Migrate(ctx, pool, ragit.FromFS(renderedSet("768"))))

	err := ragit.Migrate(ctx, pool)
	require.ErrorContains(t, err, "vector(768)")
	require.Equal(t, 768, storedDimension(t, pool))
}

// storedDimension reads the width out of the column's type, which is where
// pgvector keeps it.
func storedDimension(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var typmod int
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT a.atttypmod FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'ragit_chunks' AND a.attname = 'embedding' AND NOT a.attisdropped
	`).Scan(&typmod))
	return typmod
}
