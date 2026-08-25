// Package migrations holds ragit's schema rendered at 768 dimensions, and the
// runner to apply it.
//
// # Why this package exists at all
//
// It is the first friction point of the xberg-owned example, and it is not a
// small one. ragit.Migrate applies migrations embedded in the library, and those
// declare vector(1536) because that is what embed.DefaultDimension is. xberg's
// default local ONNX preset produces 768-component vectors. A vector(1536)
// column refuses a 768-component value — correctly, since the width is part of
// the type — so this example cannot use ragit.Migrate at all.
//
// The supported answer is to render your own set:
//
//	go run ./cmd/ragit-gen -dim 768 -migrations examples/xberg-owned/migrations -skip-models
//
// which works, and produced the .sql files beside this one. But applying them is
// then entirely the consumer's problem: ragit.Migrate is not parameterised by a
// filesystem, internal/migrate is internal, and the hand-composed RLS and
// tsvector changes live in package main inside cmd/ragit-gen where nothing can
// import them. So a consumer at any dimension other than 1536 re-implements the
// twenty lines below, including remembering the ragit_migrations table name —
// get that wrong and goose silently starts a second migration history in
// goose_db_version.
//
// The generated models need no such treatment: Chunk.Embedding is *sqlb.Vector
// whatever the width, so only the SQL differs. See docs/examples-plan.md,
// "The dimension fork".
package migrations

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// Dimension is the embedding width these migrations declare.
const Dimension = 768

// TableName must match ragit's own version table. It is not exported by the
// library — internal/migrate.TableName is internal — so it is repeated here,
// which is exactly the kind of duplication that goes stale silently.
const TableName = "ragit_migrations"

//go:embed *.sql
var FS embed.FS

// Up applies the 768-dimension schema. A reimplementation of ragit.Migrate
// differing only in where the SQL comes from.
func Up(ctx context.Context, pool *pgxpool.Pool) error {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, FS,
		goose.WithTableName(TableName))
	if err != nil {
		return fmt.Errorf("build migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply 768-dimension migrations: %w", err)
	}
	return nil
}
