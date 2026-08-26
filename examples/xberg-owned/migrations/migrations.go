// Package migrations holds ragit's schema rendered at 768 dimensions.
//
// # Why this package exists at all
//
// ragit's embedded migrations declare vector(1536), because that is
// embed.DefaultDimension. xberg's default local ONNX preset produces
// 768-component vectors, and a vector(1536) column refuses one — correctly,
// since the width is part of the type. So this example renders its own set:
//
//	go run github.com/mind-vm/ragit/cmd/ragit-gen -dim 768 \
//	    -migrations examples/xberg-owned/migrations -skip-models
//
// which produced the .sql files beside this one. The generated models need no
// such treatment: Chunk.Embedding is a *sqlb.Vector whatever the width, so only
// the SQL differs.
//
// # What used to be here
//
// A re-implementation of ragit's migration runner — twenty lines of goose
// wiring, and a copy of the ragit_migrations table name, because
// ragit.Migrate took no filesystem and internal/migrate was internal. Getting
// that name wrong is silent: goose starts a second history in its own default
// table and re-applies migrations the database already has.
//
// ragit.FromFS is that seam, so what is left here is the SQL and the width it
// declares. See docs/examples-plan.md, "The dimension fork".
package migrations

import "embed"

// Dimension is the embedding width these migrations declare. ragit refuses to
// apply them to a database created at another width rather than finding the
// version numbers already applied and quietly doing nothing.
const Dimension = 768

//go:embed *.sql
var FS embed.FS
