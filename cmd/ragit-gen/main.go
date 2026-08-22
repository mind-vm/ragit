// Command ragit-gen regenerates ragit's migrations and models from the schema
// declaration in ragitschema.
//
// It is the only thing that writes migrations/ and the *_gen.go files; both are
// checked in, so a consumer of the library needs neither this command nor sqlb's
// codegen to build.
//
//	go run ./cmd/ragit-gen
//
// A deployment that needs an embedding width other than the default renders its
// own migration set with -dim and applies it with its own runner. See
// ragit.Migrate.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jryannel/sqlb/codegen"
	"github.com/jryannel/sqlb/migrate"
	"github.com/jryannel/sqlb/schema"

	"github.com/jryannel/ragit/ragitschema"
)

func main() {
	var (
		dim        = flag.Int("dim", ragitschema.DefaultEmbeddingDimension, "embedding dimension for the vector column")
		outDir     = flag.String("migrations", "migrations", "directory to write migrations into")
		modelDir   = flag.String("models", ".", "directory to write generated models into")
		modelPkg   = flag.String("package", "ragit", "package clause for generated models")
		skipModels = flag.Bool("skip-models", false, "only render migrations")
		force      = flag.Bool("force", false, "rewrite the migrations this command owns instead of refusing")
	)
	flag.Parse()

	if err := run(*dim, *outDir, *modelDir, *modelPkg, *skipModels, *force); err != nil {
		fmt.Fprintln(os.Stderr, "ragit-gen:", err)
		os.Exit(1)
	}
}

// typeOverrides render uuid columns as uuid.UUID rather than sqlb's default
// string.
//
// An override is a rendering decision: it reaches neither the SQL type nor the
// wire, so the column stays `uuid` in Postgres either way. What it buys is that
// an id is a type rather than any string that happens to be lying around —
// uuid.UUID cannot hold "hello", and a mixed-up argument order fails to compile
// instead of failing at the database.
var typeOverrides = []codegen.TypeOverride{
	{Type: schema.TypeUUID, GoType: "uuid.UUID", Import: "github.com/google/uuid"},
}

// The migrations this command owns, and may therefore rewrite under -force.
const (
	initialMigrationName     = "initial_schema"
	handwrittenMigrationName = "search_vector_and_rls"
	initialMigration         = "00001_" + initialMigrationName + ".sql"
	handwrittenMigration     = "00002_" + handwrittenMigrationName + ".sql"
)

func run(dim int, outDir, modelDir, modelPkg string, skipModels, force bool) error {
	s := ragitschema.New(dim)
	if err := s.Registry.Validate(); err != nil {
		return fmt.Errorf("schema is invalid: %w", err)
	}

	if err := writeMigrations(s, outDir, force); err != nil {
		return err
	}
	if skipModels {
		return nil
	}

	files, err := codegen.Generate(codegen.Options{
		Registry:     s.Registry,
		Dir:          modelDir,
		Package:      modelPkg,
		ModelsFile:   "models_gen.go",
		ColumnsFile:  "columns_gen.go",
		ManifestFile: "-",
		RestFile:     "-",
		Types:        typeOverrides,
	})
	if err != nil {
		return fmt.Errorf("generate models: %w", err)
	}
	for _, f := range files {
		fmt.Println("wrote", f)
	}
	return nil
}

// writeMigrations renders the whole schema as one initial migration, followed
// by the pieces sqlb's DDL layer cannot express.
func writeMigrations(s *ragitschema.Schema, outDir string, force bool) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}

	// sqlb refuses to overwrite a migration, because a history that has been
	// applied somewhere is append-only. That is the right default and the
	// wrong one while ragit's initial schema is still being authored, so
	// -force removes the two files this command owns and lets it rewrite
	// them. It never touches a file it did not generate.
	if force {
		for _, name := range []string{initialMigration, handwrittenMigration} {
			if err := os.Remove(filepath.Join(outDir, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("clear %s: %w", name, err)
			}
		}
	}

	changes, err := migrate.Diff(schema.NewRegistry(), s.Registry)
	if err != nil {
		return fmt.Errorf("diff schema: %w", err)
	}

	// sqlb emits CREATE EXTENSION "vector" itself, ahead of the column that
	// needs it, so nothing has to be prepended here. gen_random_uuid() is
	// built in from PostgreSQL 13 and needs no extension.
	written, err := migrate.Write(outDir, migrate.Migration{
		Version: migrate.SequentialVersion(1),
		Name:    initialMigrationName,
		Changes: changes,
	}, migrate.Options{})
	if err != nil {
		return fmt.Errorf("write initial migration: %w", err)
	}

	// Hand-composed changes go through the same renderer so they get the same
	// formatting and statement splitting, and are marked Handwritten so the
	// file does not claim to be something sqlb's emitters produced.
	extra, err := migrate.Write(outDir, migrate.Migration{
		Version: migrate.SequentialVersion(2),
		Name:    handwrittenMigrationName,
		Changes: append(searchVectorChanges(s), rlsChanges(s)...),
	}, migrate.Options{Handwritten: true})
	if err != nil {
		return fmt.Errorf("write handwritten migration: %w", err)
	}

	for _, f := range append(written, extra...) {
		fmt.Println("wrote", filepath.Clean(f))
	}
	return nil
}
