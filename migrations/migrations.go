// Package migrations embeds ragit's schema migrations so a host application
// never has to vendor the SQL into its own migration sequence.
//
// ragit owns its own migration line, tracked in its own version table
// (see [github.com/mind-vm/ragit.Migrate]), exactly as River does with
// river_migration. A host app can therefore run its own goose/atlas/whatever
// sequence over its own tables and upgrade ragit independently, without the
// two ever colliding on a version number.
package migrations

import "embed"

// FS holds the goose-formatted migration files.
//
//go:embed *.sql
var FS embed.FS
