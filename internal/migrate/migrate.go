// Package migrate applies ragit's embedded schema migrations.
//
// It exists as its own package so that both the public entry point
// (ragit.Migrate) and the test harness can use it without the test harness
// having to import the root package it is used to test.
package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/mind-vm/ragit/migrations"
)

// TableName is ragit's own schema-version table. It is deliberately not
// goose's default goose_db_version: a host application running its own goose
// sequence over its own tables must be able to upgrade ragit independently,
// without the two lines colliding on a version number. Same reasoning behind
// River's separate river_migration table.
const TableName = "ragit_migrations"

// Up applies every outstanding migration.
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	return run(ctx, pool, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// Down rolls back the most recent migration.
func Down(ctx context.Context, pool *pgxpool.Pool) error {
	return run(ctx, pool, func(p *goose.Provider) error {
		_, err := p.Down(ctx)
		return err
	})
}

func run(ctx context.Context, pool *pgxpool.Pool, apply func(*goose.Provider) error) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations.FS,
		goose.WithTableName(TableName))
	if err != nil {
		return fmt.Errorf("ragit: build migration provider: %w", err)
	}
	if err := apply(provider); err != nil {
		return fmt.Errorf("ragit: apply migrations: %w", err)
	}
	return nil
}
