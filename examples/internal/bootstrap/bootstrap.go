// Package bootstrap brings up everything both examples need: a migrated
// database, an unprivileged role to connect as, and an object store.
//
// It is shared so the two examples differ only in pipeline shape. Everything
// here would live in a host application's own startup path — this is not
// library code ragit ships, and the fact that every consumer would have to
// write the role-creation part of it is itself one of the questions these
// examples exist to answer (docs/examples-plan.md, shape question 5).
package bootstrap

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jryannel/sqlb/introspect"
	"github.com/jryannel/sqlb/migrate"

	"github.com/jryannel/ragit"
	"github.com/jryannel/ragit/store"
)

// Env is a brought-up environment.
type Env struct {
	Cfg Config

	// Admin is a superuser pool with no pgvector codec registered. Migrations
	// have to run on a pool like this: CREATE EXTENSION vector is itself one
	// of them, so a codec needing the extension's OID cannot be registered
	// before they run.
	Admin *pgxpool.Pool

	// App is what an example actually uses: the unprivileged role, with the
	// pgvector codec registered. RLS applies to this pool and not to Admin.
	App *pgxpool.Pool

	// Store is the object store for original document bytes.
	Store *store.MinIOStore
}

// Close releases both pools.
func (e *Env) Close() {
	if e.App != nil {
		e.App.Close()
	}
	if e.Admin != nil {
		e.Admin.Close()
	}
}

// Setup migrates, creates the application role, and returns pools for both.
//
// The order is load-bearing. Migrations run as the superuser, so it owns the
// tables; the application role is created afterwards as a plain grantee, which
// is what makes ragit's FORCE ROW LEVEL SECURITY policies apply to it. Do this
// the other way round — or skip it and connect as the superuser — and every
// policy is silently inert while every test of them still passes.
func Setup(ctx context.Context, cfg Config) (*Env, error) {
	env := &Env{Cfg: cfg}

	admin, err := pgxpool.New(ctx, cfg.AdminDSN)
	if err != nil {
		return nil, fmt.Errorf("connect as admin: %w", err)
	}
	env.Admin = admin

	if err := admin.Ping(ctx); err != nil {
		env.Close()
		return nil, fmt.Errorf("ping postgres (is `make up` running?): %w", err)
	}

	// ragit's migration line, tracked in ragit_migrations.
	if err := ragit.Migrate(ctx, admin); err != nil {
		env.Close()
		return nil, err
	}

	// The host application's own, entirely separate. Neither knows the
	// other's version numbers, which is what the ragit_ prefix buys.
	if err := MigrateDemoSchema(ctx, admin); err != nil {
		env.Close()
		return nil, err
	}

	if err := ensureAppRole(ctx, admin, cfg); err != nil {
		env.Close()
		return nil, err
	}

	appDSN, err := cfg.AppDSN()
	if err != nil {
		env.Close()
		return nil, err
	}
	app, err := ragit.NewPool(ctx, appDSN)
	if err != nil {
		env.Close()
		return nil, err
	}
	env.App = app

	if err := app.Ping(ctx); err != nil {
		env.Close()
		return nil, fmt.Errorf("connect as %s: %w", cfg.AppRole, err)
	}

	st, err := store.NewMinIOStore(ctx, store.MinIOConfig{
		Endpoint:  cfg.MinIOEndpoint,
		AccessKey: cfg.MinIOAccessKey,
		SecretKey: cfg.MinIOSecretKey,
		Bucket:    cfg.MinIOBucket,
	})
	if err != nil {
		env.Close()
		return nil, err
	}
	env.Store = st

	return env, nil
}

// MigrateDemoSchema brings the host application's own tables up to their
// declaration, by diffing what the database has against what it should have.
//
// A real application would render these changes to files and hand them to its
// own runner — sqlb generates migrations, it does not apply them. Applying the
// diff directly is an example's shortcut, and it is what makes this function
// idempotent: on the second run the diff is empty and nothing executes.
func MigrateDemoSchema(ctx context.Context, pool *pgxpool.Pool) error {
	target := NewDemoSchema()
	if err := target.Registry.Validate(); err != nil {
		return fmt.Errorf("demo schema is invalid: %w", err)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	// Only the demo module's tables. Introspecting everything would import
	// ragit's tables and River's too, and diffing a declaration of one table
	// against an import of many reports the rest as tables to drop.
	current, _, err := introspect.Registry(ctx, conn.Conn(), introspect.Options{
		Module: DemoModule,
		Only:   []string{Upload{}.TableName()},
	})
	if err != nil {
		return fmt.Errorf("introspect demo schema: %w", err)
	}

	changes, err := migrate.Diff(current, target.Registry)
	if err != nil {
		return fmt.Errorf("diff demo schema: %w", err)
	}
	for _, c := range changes {
		if _, err := pool.Exec(ctx, c.Up); err != nil {
			return fmt.Errorf("apply demo change %q: %w", c.Comment, err)
		}
	}
	return nil
}

// ensureAppRole creates the unprivileged role and grants it what an
// application needs, without letting it near SUPERUSER or BYPASSRLS.
//
// Both attributes are named explicitly rather than left to default. They are
// the difference between row-level security that works and row-level security
// that is decorative, and a default is not the place to leave that.
func ensureAppRole(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	// The role name is validated as a plain identifier in LoadConfig;
	// PostgreSQL cannot parameterise one. The password is a literal, so it
	// goes through quote_literal rather than being interpolated.
	create := fmt.Sprintf(`
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = %s) THEN
        EXECUTE format('CREATE ROLE %s LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD %%L', %s);
    END IF;
END
$$;`, quote(cfg.AppRole), cfg.AppRole, quote(cfg.AppPassword))

	stmts := []string{
		create,
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", cfg.AppRole),
		fmt.Sprintf("GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", cfg.AppRole),
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", cfg.AppRole),
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("grant app role: %w", err)
		}
	}
	return nil
}

// IsSuperuser reports whether the role a pool connects as bypasses RLS.
//
// Exported because it is worth asserting at startup rather than discovering
// from an isolation test that passes for the wrong reason.
func IsSuperuser(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	var super, bypass bool
	err := pool.QueryRow(ctx,
		"SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user").
		Scan(&super, &bypass)
	if err != nil {
		return false, fmt.Errorf("read role attributes: %w", err)
	}
	return super || bypass, nil
}

// quote renders a Go string as a PostgreSQL string literal. Used for the two
// values in the role DDL that are literals rather than identifiers.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func rewriteUser(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
