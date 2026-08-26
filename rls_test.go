package ragit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mind-vm/sqlb"
	"github.com/stretchr/testify/require"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/internal/testutil"
)

const appPassword = "granted_pw"

// createRole makes a login role with the attributes the docs tell a consumer
// to use. Roles are cluster-wide, so the name is unique per test.
func createRole(t *testing.T, pool *pgxpool.Pool, attributes string) string {
	t.Helper()
	role := "app_" + uuid.NewString()[:8]
	_, err := pool.Exec(context.Background(),
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' %s", role, appPassword, attributes))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "REASSIGN OWNED BY "+role+" TO CURRENT_USER")
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+role)
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	})
	return role
}

// TestGrantAppRole_GivesAWorkingConfinedRole is shape question 5: the twenty
// lines every consumer was left to rediscover, and the trap underneath them.
func TestGrantAppRole_GivesAWorkingConfinedRole(t *testing.T) {
	admin := testutil.SetupScratchDatabase(t)
	ctx := context.Background()
	require.NoError(t, ragit.Migrate(ctx, admin))

	role := createRole(t, admin, "NOSUPERUSER NOBYPASSRLS")
	require.NoError(t, ragit.GrantAppRole(ctx, admin, role))

	app := testutil.ConnectScratchAs(t, admin, role, appPassword)

	// The grant is enough to run the library.
	require.NoError(t, ragit.VerifyRLS(ctx, app))

	tenantA, tenantB := uuid.New(), uuid.New()
	insertDocument(t, app, tenantA, "a.md")

	// And the confinement is the database's: tenant B's transaction cannot see
	// tenant A's row even with no predicate naming a tenant at all.
	require.Equal(t, 1, countDocuments(t, app, tenantA))
	require.Equal(t, 0, countDocuments(t, app, tenantB),
		"a query with no tenant predicate must still be confined by the policy")
}

// TestGrantAppRole_RefusesARoleTheseGrantsCannotConfine is the trap itself. A
// superuser reads every tenant's rows while every Scope-based test still
// passes, because the predicates alone are doing the work.
func TestGrantAppRole_RefusesARoleTheseGrantsCannotConfine(t *testing.T) {
	admin := testutil.SetupScratchDatabase(t)
	ctx := context.Background()
	require.NoError(t, ragit.Migrate(ctx, admin))

	super := createRole(t, admin, "SUPERUSER")
	err := ragit.GrantAppRole(ctx, admin, super)
	require.ErrorIs(t, err, ragit.ErrRLSNotEnforced)
	require.ErrorContains(t, err, "SUPERUSER")

	bypass := createRole(t, admin, "NOSUPERUSER BYPASSRLS")
	err = ragit.GrantAppRole(ctx, admin, bypass)
	require.ErrorIs(t, err, ragit.ErrRLSNotEnforced)
	require.ErrorContains(t, err, "BYPASSRLS")

	require.ErrorContains(t, ragit.GrantAppRole(ctx, admin, "no_such_role_here"), "does not exist")
}

// TestVerifyRLS_ReportsAnExemptConnection is the startup check a host
// application is meant to run on its own pool.
func TestVerifyRLS_ReportsAnExemptConnection(t *testing.T) {
	admin := testutil.SetupScratchDatabase(t)
	ctx := context.Background()
	require.NoError(t, ragit.Migrate(ctx, admin))

	// The admin pool is the stock postgres image's superuser — the exact
	// connection a consumer reaches for first, and the one whose isolation is
	// entirely imaginary.
	err := ragit.VerifyRLS(ctx, admin)
	require.ErrorIs(t, err, ragit.ErrRLSNotEnforced)
	require.ErrorContains(t, err, "SUPERUSER")

	bypass := createRole(t, admin, "NOSUPERUSER BYPASSRLS")
	_, err = admin.Exec(ctx, fmt.Sprintf(
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ragit_documents, ragit_chunks TO %s", bypass))
	require.NoError(t, err)
	require.ErrorIs(t, ragit.VerifyRLS(ctx, testutil.ConnectScratchAs(t, admin, bypass, appPassword)),
		ragit.ErrRLSNotEnforced)
}

// TestVerifyRLS_ReportsUnprotectedTables covers the other half: the right role
// against tables whose policies are not there, which reads identically to a
// working setup until someone else's rows show up.
func TestVerifyRLS_ReportsUnprotectedTables(t *testing.T) {
	admin := testutil.SetupScratchDatabase(t)
	ctx := context.Background()

	role := createRole(t, admin, "NOSUPERUSER NOBYPASSRLS")
	app := testutil.ConnectScratchAs(t, admin, role, appPassword)

	// Nothing migrated yet.
	err := ragit.VerifyRLS(ctx, app)
	require.ErrorIs(t, err, ragit.ErrRLSNotEnforced)
	require.ErrorContains(t, err, "does not exist")

	require.NoError(t, ragit.Migrate(ctx, admin))
	require.NoError(t, ragit.GrantAppRole(ctx, admin, role))
	require.NoError(t, ragit.VerifyRLS(ctx, app))

	// FORCE is what keeps the owner from reading past the policies. Dropping
	// it leaves every policy in place and still reported as a problem.
	_, err = admin.Exec(ctx, "ALTER TABLE ragit_chunks NO FORCE ROW LEVEL SECURITY")
	require.NoError(t, err)
	err = ragit.VerifyRLS(ctx, app)
	require.ErrorIs(t, err, ragit.ErrRLSNotEnforced)
	require.ErrorContains(t, err, "ragit_chunks does not have FORCE ROW LEVEL SECURITY")
}

func insertDocument(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, filename string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, ragit.WithTenant(ctx, pool, tenantID, func(db sqlb.Executor) error {
		_, err := sqlb.InsertRows(&ragit.Document{
			TenantID: tenantID, Filename: filename, MimeType: "text/markdown",
			Metadata: json.RawMessage("{}"), Attributes: json.RawMessage("{}"),
		}).Omit("id", "created_at", "updated_at", "status", "metadata").Exec(ctx, db)
		return err
	}))
}

// countDocuments counts without naming a tenant, so what comes back is the
// policy's answer rather than the predicate's.
func countDocuments(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	var n int64
	require.NoError(t, ragit.WithTenant(ctx, pool, tenantID, func(db sqlb.Executor) error {
		var err error
		n, err = sqlb.Query[ragit.Document]().Count(ctx, db)
		return err
	}))
	return int(n)
}
