// Package migrate applies ragit's schema migrations.
//
// It exists as its own package so that both the public entry point
// (ragit.Migrate) and the test harness can use it without the test harness
// having to import the root package it is used to test.
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

// TableName is ragit's own schema-version table. It is deliberately not
// goose's default goose_db_version: a host application running its own goose
// sequence over its own tables must be able to upgrade ragit independently,
// without the two lines colliding on a version number. Same reasoning behind
// River's separate river_migration table.
const TableName = "ragit_migrations"

// Up applies every outstanding migration in fsys.
func Up(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	return run(ctx, pool, fsys, func(p *goose.Provider) error {
		_, err := p.Up(ctx)
		return err
	})
}

// Down rolls back the most recent migration in fsys.
func Down(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	return run(ctx, pool, fsys, func(p *goose.Provider) error {
		_, err := p.Down(ctx)
		return err
	})
}

func run(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, apply func(*goose.Provider) error) error {
	if err := checkDimension(ctx, pool, fsys); err != nil {
		return err
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithTableName(TableName))
	if err != nil {
		return fmt.Errorf("ragit: build migration provider: %w", err)
	}
	if err := apply(provider); err != nil {
		return fmt.Errorf("ragit: apply migrations: %w", err)
	}
	return nil
}

// checkDimension refuses a migration set whose embedding width disagrees with
// the database's.
//
// Without it this is the quietest failure in the whole library. Every rendered
// set carries the same version numbers whatever `-dim` produced it, so goose
// looks at a database migrated at one width, sees those versions already
// applied, and reports success having done nothing at all. The column keeps
// its old width, and the first insert of a vector fails far from here — or, if
// the corpus is still empty, retrieval simply never matches.
func checkDimension(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	declared, ok := declaredDimension(fsys)
	if !ok {
		return nil
	}
	stored, ok, err := storedDimension(ctx, pool)
	if err != nil || !ok {
		return err
	}
	if declared != stored {
		return fmt.Errorf(
			"ragit: these migrations declare vector(%d) but %s.embedding is vector(%d); "+
				"a vector column's width is part of its type, so one database cannot hold both — "+
				"render a set at %d with `ragit-gen -dim %d`, or point this at a different database",
			declared, chunkTable, stored, stored, stored)
	}
	return nil
}

const chunkTable = "ragit_chunks"

// vectorType matches the width in a generated column declaration. The SQL it
// reads is ragit's own, rendered by cmd/ragit-gen from one schema declaration,
// so there is exactly one shape to match.
var vectorType = regexp.MustCompile(`(?i)vector\((\d+)\)`)

// declaredDimension is the embedding width a migration set creates, and
// whether it says. A set that declares none is not ragit's, or is a partial
// set that only alters other columns; either way there is nothing to compare.
func declaredDimension(fsys fs.FS) (int, bool) {
	files, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return 0, false
	}
	for _, name := range files {
		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		if m := vectorType.FindSubmatch(content); m != nil {
			dim := 0
			for _, c := range m[1] {
				dim = dim*10 + int(c-'0')
			}
			return dim, dim > 0
		}
	}
	return 0, false
}

// storedDimension is the width of the chunk vector column as the database has
// it, and whether the column exists at all. pgvector keeps the dimension in
// the type modifier, so this reads the type rather than any bookkeeping that
// could disagree with it.
func storedDimension(ctx context.Context, pool *pgxpool.Pool) (int, bool, error) {
	const q = `SELECT a.atttypmod
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $1 AND a.attname = 'embedding'
		  AND NOT a.attisdropped AND n.nspname = current_schema()`

	var typmod int
	err := pool.QueryRow(ctx, q, chunkTable).Scan(&typmod)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("ragit: read stored embedding dimension: %w", err)
	case typmod <= 0:
		// An unconstrained `vector` column: no width to disagree with.
		return 0, false, nil
	}
	return typmod, true, nil
}
