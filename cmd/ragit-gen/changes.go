package main

import (
	"fmt"

	"github.com/mind-vm/sqlb/migrate"

	"github.com/mind-vm/ragit/ragitschema"
)

// searchVectorChanges adds the stored tsvector column backing full-text search.
//
// sqlb's schema DSL has no GENERATED ALWAYS ... STORED column, and its own
// Searchable capability compiles to ILIKE '%…%', which is a different feature:
// no btree or GIN-trigram index makes it a ranked full-text search, and it
// cannot express websearch_to_tsquery's grammar. So this stays hand-written.
//
// The 'simple' configuration is not a detail. It must match the config used at
// query time in search/: an 'english' column stores stemmed lexemes, and
// matching those against unstemmed query terms under-matches *silently*,
// with no error to notice.
func searchVectorChanges(s *ragitschema.Schema) []migrate.Change {
	table := s.Chunk.Name()
	return []migrate.Change{
		{
			Up: fmt.Sprintf(
				"ALTER TABLE %s ADD COLUMN search_vector tsvector\n"+
					"    GENERATED ALWAYS AS (to_tsvector('simple', content)) STORED;", table),
			Down: fmt.Sprintf("ALTER TABLE %s DROP COLUMN search_vector;", table),
		},
		{
			Up:   fmt.Sprintf("CREATE INDEX idx_%s_search_vector ON %s USING gin(search_vector);", table, table),
			Down: fmt.Sprintf("DROP INDEX IF EXISTS idx_%s_search_vector;", table),
		},
	}
}

// rlsChanges enables row-level security on both tables.
//
// This is defence in depth beneath sqlb's BeforeQuery confinement, not a
// replacement for it. The hook constrains every query the engine builds; RLS
// constrains everything else — a raw pgx call, a psql session, a future query
// written against these tables by code that never registered a hook. Neither
// layer subsumes the other, and ragit's tables live inside a host
// application's database where "everything else" is a real population.
//
// Two properties worth knowing before relying on it:
//
//   - FORCE is required. Without it the table owner — which is what many
//     applications connect as — bypasses the policy entirely.
//   - PostgreSQL exempts superusers and BYPASSRLS roles regardless, FORCE or
//     not. The stock postgres image's POSTGRES_USER is a superuser, so an
//     application connecting as it sees these policies silently do nothing.
//     Connect as an ordinary role.
//
// NULLIF guards the empty string: current_setting(..., true) yields NULL when
// the GUC was never set but ” when it was set and reset, and ”::uuid errors.
// Either way the comparison is NULL, which is not true, so it fails closed.
func rlsChanges(s *ragitschema.Schema) []migrate.Change {
	var out []migrate.Change
	for _, table := range []string{s.Document.Name(), s.Chunk.Name()} {
		policy := table + "_tenant_isolation"
		out = append(out, migrate.Change{
			Up: fmt.Sprintf(
				"ALTER TABLE %[1]s ENABLE ROW LEVEL SECURITY;\n"+
					"ALTER TABLE %[1]s FORCE ROW LEVEL SECURITY;\n"+
					"CREATE POLICY %[2]s ON %[1]s\n"+
					"    USING (\n"+
					"        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid\n"+
					"        OR current_setting('ragit.maintenance', true) = 'on'\n"+
					"    )\n"+
					"    WITH CHECK (\n"+
					"        tenant_id = NULLIF(current_setting('ragit.tenant_id', true), '')::uuid\n"+
					"    );", table, policy),
			Down: fmt.Sprintf(
				"DROP POLICY IF EXISTS %[2]s ON %[1]s;\n"+
					"ALTER TABLE %[1]s NO FORCE ROW LEVEL SECURITY;\n"+
					"ALTER TABLE %[1]s DISABLE ROW LEVEL SECURITY;", table, policy),
		})
	}
	return out
}
