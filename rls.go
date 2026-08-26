package ragit

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrRLSNotEnforced reports that ragit's row-level security is not actually
// confining the role a pool connects as. See [VerifyRLS].
var ErrRLSNotEnforced = errors.New("ragit: row-level security is not enforced for this connection")

// GrantAppRole grants role what an application needs on ragit's tables, and
// nothing else.
//
// Run it as the role that owns the tables, after [Migrate]:
//
//	if err := ragit.Migrate(ctx, adminPool); err != nil { return err }
//	if err := ragit.GrantAppRole(ctx, adminPool, "myapp"); err != nil { return err }
//
// Creating the role is deliberately not part of this. A role needs a password,
// and where that comes from is a deployment's business, not a library's. What
// the role must not have is SUPERUSER or BYPASSRLS — PostgreSQL exempts both
// from row-level security entirely, FORCE or not, so an application connecting
// as one has every policy here silently doing nothing. Granting to such a role
// is refused rather than quietly wasted:
//
//	CREATE ROLE myapp LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD '…';
//
// This grants on ragit's own tables only. A host application's tables are its
// own to grant, and a library handing out privileges across a schema it does
// not own would be a poor guest. ragit's version table is not included either:
// migrations run as the owner, and an application role has no business
// writing schema history.
//
// Call it again after any [Migrate] that adds a table. GRANT is not a standing
// rule — it applies to what exists when it runs.
func GrantAppRole(ctx context.Context, pool *pgxpool.Pool, role string) error {
	if strings.TrimSpace(role) == "" {
		return errors.New("ragit: GrantAppRole needs a role name")
	}
	exempt, err := roleIsExempt(ctx, pool, role)
	if err != nil {
		return err
	}
	if exempt != "" {
		return fmt.Errorf("%w: role %q is %s, so ragit's policies would not apply to it; "+
			"grant to a role created NOSUPERUSER NOBYPASSRLS instead", ErrRLSNotEnforced, role, exempt)
	}

	// Neither a role name nor a schema name can be a query parameter, so both
	// go through pgx's identifier quoting rather than into the string raw.
	quoted := pgx.Identifier{role}.Sanitize()
	var schema string
	if err := pool.QueryRow(ctx, "SELECT current_schema()").Scan(&schema); err != nil {
		return fmt.Errorf("ragit: read current schema: %w", err)
	}

	stmts := []string{
		fmt.Sprintf("GRANT USAGE ON SCHEMA %s TO %s", pgx.Identifier{schema}.Sanitize(), quoted),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON %s, %s TO %s",
			pgx.Identifier{Document{}.TableName()}.Sanitize(),
			pgx.Identifier{Chunk{}.TableName()}.Sanitize(), quoted),
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("ragit: grant to %s (%q): %w", role, stmt, err)
		}
	}
	return nil
}

// VerifyRLS reports whether tenant isolation on this connection is the
// database's doing or only the query's.
//
// It answers one question — would ragit's policies actually stop a query that
// forgot its confinement? — and it is worth asking at startup, because every
// way of getting this wrong is silent. A superuser or BYPASSRLS role reads
// every tenant's rows while every test that goes through [Scope] still passes,
// since the predicates alone are doing the work. Tables missing FORCE ROW
// LEVEL SECURITY leak to their owner in the same way.
//
// It is the counterpart of [Processor.CountMisalignedChunks]: a state that
// cannot be noticed by using the library normally, made detectable
// deliberately.
//
//	if err := ragit.VerifyRLS(ctx, pool); err != nil {
//	    return err // refuse to serve rather than serve unconfined
//	}
//
// Errors wrap [ErrRLSNotEnforced]. Run it on the application's own pool: it
// reports on the role that pool connects as, so a migration pool answering as
// a superuser is expected and says nothing about the application's.
func VerifyRLS(ctx context.Context, pool *pgxpool.Pool) error {
	var role string
	if err := pool.QueryRow(ctx, "SELECT current_user").Scan(&role); err != nil {
		return fmt.Errorf("ragit: read current role: %w", err)
	}

	var problems []string
	exempt, err := roleIsExempt(ctx, pool, role)
	if err != nil {
		return err
	}
	if exempt != "" {
		problems = append(problems, fmt.Sprintf(
			"the connected role %q is %s, and PostgreSQL exempts it from every policy", role, exempt))
	}

	unforced, err := tablesWithoutForcedRLS(ctx, pool)
	if err != nil {
		return err
	}
	problems = append(problems, unforced...)

	if len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrRLSNotEnforced, strings.Join(problems, "; "))
	}
	return nil
}

// roleIsExempt names the attribute that would put a role outside row-level
// security, or "" if it has neither. Role attributes are not inherited through
// membership, so the role's own row is the whole answer.
func roleIsExempt(ctx context.Context, pool *pgxpool.Pool, role string) (string, error) {
	var super, bypass bool
	err := pool.QueryRow(ctx,
		"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = $1", role).Scan(&super, &bypass)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("ragit: role %q does not exist", role)
	case err != nil:
		return "", fmt.Errorf("ragit: read role %q: %w", role, err)
	case super:
		return "a SUPERUSER", nil
	case bypass:
		return "BYPASSRLS", nil
	}
	return "", nil
}

// tablesWithoutForcedRLS lists ragit tables whose row-level security is off or
// unforced, and reports a missing table as unprotected rather than absent —
// there is no safe reading of "the policies are not there".
func tablesWithoutForcedRLS(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	tables := []string{Document{}.TableName(), Chunk{}.TableName()}

	const q = `SELECT c.relrowsecurity AND c.relforcerowsecurity
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relname = $1 AND n.nspname = current_schema()`

	var out []string
	for _, table := range tables {
		var forced bool
		err := pool.QueryRow(ctx, q, table).Scan(&forced)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			out = append(out, table+" does not exist; run ragit.Migrate")
		case err != nil:
			return nil, fmt.Errorf("ragit: read row-level security of %s: %w", table, err)
		case !forced:
			out = append(out, table+" does not have FORCE ROW LEVEL SECURITY; run ragit.Migrate")
		}
	}
	return out, nil
}
