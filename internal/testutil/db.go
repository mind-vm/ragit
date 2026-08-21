// Package testutil boots a real, migrated Postgres for integration tests.
//
// Simplified relative to a larger, longer-running test suite's needs: one
// shared pgvector-enabled container per test binary, migrated once, with
// tables truncated between tests rather than a database cloned per test.
// Worth revisiting (e.g. a template-database clone-per-test, for full
// parallelism) if the suite grows enough for truncation contention to
// matter — not needed at this scale.
package testutil

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	once     sync.Once
	connStr  string
	setupErr error
)

// SetupTestPool returns a pgxpool.Pool connected to a shared, migrated
// Postgres, starting the container on first use. It truncates the
// documents/chunks tables via t.Cleanup so each test starts clean.
//
// Skips under -short, so `go test -short ./...` needs no Docker.
func SetupTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("needs Postgres; skipped in -short mode")
	}

	once.Do(func() {
		connStr, setupErr = startAndMigrate()
	})
	if setupErr != nil {
		t.Fatalf("testutil: setup shared postgres: %v", setupErr)
	}

	ctx := context.Background()
	poolConfig, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		t.Fatalf("testutil: parse connection string: %v", err)
	}
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvector.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("testutil: connect: %v", err)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "TRUNCATE documents CASCADE")
		pool.Close()
	})

	return pool
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

	connStr, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", fmt.Errorf("get connection string: %w", err)
	}

	migrationsDir, err := findMigrationsDir()
	if err != nil {
		return "", err
	}

	sqlDB, err := sql.Open("pgx", connStr)
	if err != nil {
		return "", fmt.Errorf("open sql.DB: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	goose.SetLogger(goose.NopLogger())
	if err := goose.Up(sqlDB, migrationsDir); err != nil {
		return "", fmt.Errorf("run migrations: %w", err)
	}

	return connStr, nil
}

// findMigrationsDir walks up from the working directory to locate the
// module's migrations/ directory — needed because tests run with their
// package directory as the working directory.
func findMigrationsDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "migrations")
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("migrations directory not found walking up from working directory")
		}
		dir = parent
	}
}
