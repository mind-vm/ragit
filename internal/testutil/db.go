// Package testutil boots a real, migrated Postgres for integration tests.
//
// Simplified relative to a larger, longer-running test suite's needs: one
// shared pgvector-enabled container per test binary, migrated once, with
// tables truncated between tests rather than a database cloned per test.
// Worth revisiting (e.g. a template-database clone-per-test, for full
// parallelism) if the suite grows enough for truncation contention to
// matter — not needed at this scale.
//
// Tests connect as a deliberately unprivileged role, not as the superuser
// the postgres image creates. That is load-bearing rather than tidiness:
// PostgreSQL exempts superusers from row-level security even when the table
// is FORCE'd, so a suite connecting as the default POSTGRES_USER would see
// every RLS policy silently do nothing and would pass whether or not tenant
// isolation actually works.
package testutil

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jryannel/ragit/internal/migrate"
)

// appRole is the unprivileged role tests connect as. It owns nothing and has
// neither SUPERUSER nor BYPASSRLS, so the policies in migration 00001 apply
// to it exactly as they would to a production application role.
const (
	appRole     = "ragit_app"
	appPassword = "ragit_app_pw"
)

var (
	once         sync.Once
	appConnStr   string
	adminConnStr string
	setupErr     error
)

// SetupTestPool returns a pgxpool.Pool connected to a shared, migrated
// Postgres as the unprivileged application role, starting the container on
// first use. It truncates the ragit tables via t.Cleanup so each test starts
// clean.
//
// Skips under -short, so `go test -short ./...` needs no Docker.
func SetupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("needs Postgres; skipped in -short mode")
	}

	once.Do(func() {
		appConnStr, setupErr = startAndMigrate()
	})
	if setupErr != nil {
		t.Fatalf("testutil: setup shared postgres: %v", setupErr)
	}

	pool, err := connect(context.Background(), appConnStr)
	if err != nil {
		t.Fatalf("testutil: connect as %s: %v", appRole, err)
	}

	t.Cleanup(func() {
		// TRUNCATE is a privilege check, not a row-level one, so it is not
		// filtered by RLS and clears every tenant's rows regardless of which
		// tenant GUC happens to be set.
		_, _ = pool.Exec(context.Background(), "TRUNCATE ragit_documents CASCADE")
		pool.Close()
	})

	return pool
}

// connect registers pgvector's types on every connection, which requires the
// vector extension to already exist — so it is only usable after migrations.
func connect(ctx context.Context, connStr string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, fmt.Errorf("parse connection string: %w", err)
	}
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	return pgxpool.NewWithConfig(ctx, poolConfig)
}

// connectBare skips pgvector type registration. The migration pool has to
// use this: CREATE EXTENSION vector is itself one of the migrations, so a
// connection that insists on resolving the vector type up front cannot be
// opened until after it has run.
func connectBare(ctx context.Context, connStr string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return pool, nil
}

func startAndMigrate() (string, error) {
	ctx := context.Background()

	pgContainer, err := tcpostgres.Run(ctx,
		"pgvector/pgvector:pg18",
		tcpostgres.WithDatabase("ragit_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		testcontainers.WithCmdArgs(
			"-c", "synchronous_commit=off",
			"-c", "full_page_writes=off",
			"-c", "fsync=off",
		),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		return "", fmt.Errorf("start postgres container: %w", err)
	}

	adminConnStr, err = pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("get connection string: %w", err)
	}

	adminPool, err := connectBare(ctx, adminConnStr)
	if err != nil {
		return "", fmt.Errorf("connect as admin: %w", err)
	}
	defer adminPool.Close()

	// Migrations run as the superuser, so it owns the tables; the app role
	// created below is a plain grantee and RLS therefore applies to it.
	if err := migrate.Up(ctx, adminPool); err != nil {
		return "", err
	}
	if err := grantAppRole(ctx, adminPool); err != nil {
		return "", err
	}

	return rewriteCredentials(adminConnStr, appRole, appPassword)
}

// SetupScratchDatabase creates a fresh, empty database inside the shared
// container and returns an admin pool to it.
//
// It exists so a test can run migrations — including rolling them back —
// without touching the migrated database every other test shares. Rolling
// back in the shared database would drop columns out from under whatever
// happens to run next.
func SetupScratchDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("needs Postgres; skipped in -short mode")
	}

	once.Do(func() {
		appConnStr, setupErr = startAndMigrate()
	})
	if setupErr != nil {
		t.Fatalf("testutil: setup shared postgres: %v", setupErr)
	}

	ctx := context.Background()
	adminPool, err := connectBare(ctx, adminConnStr)
	if err != nil {
		t.Fatalf("testutil: connect as admin: %v", err)
	}
	defer adminPool.Close()

	// Database names cannot be parameterised, so this is interpolated — with
	// a name this package generates rather than anything caller-supplied.
	name := "scratch_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testutil: create scratch database: %v", err)
	}

	scratchConnStr, err := rewriteDatabase(adminConnStr, name)
	if err != nil {
		t.Fatalf("testutil: build scratch connection string: %v", err)
	}
	pool, err := connectBare(ctx, scratchConnStr)
	if err != nil {
		t.Fatalf("testutil: connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)

	return pool
}

func rewriteDatabase(connStr, database string) (string, error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return "", fmt.Errorf("parse connection url: %w", err)
	}
	u.Path = "/" + database
	return u.String(), nil
}

func grantAppRole(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD '%s' NOSUPERUSER NOBYPASSRLS", appRole, appPassword),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", appRole),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON ALL TABLES IN SCHEMA public TO %s", appRole),
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", appRole),
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("grant app role (%q): %w", stmt, err)
		}
	}
	return nil
}

func rewriteCredentials(connStr, user, password string) (string, error) {
	u, err := url.Parse(connStr)
	if err != nil {
		return "", fmt.Errorf("parse connection url: %w", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
