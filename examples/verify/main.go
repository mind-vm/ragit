// Command verify checks that the shared example environment is actually the
// environment the examples assume.
//
// It exists because the two most important properties here fail silently. Row-
// level security is inert for a superuser, so an example connecting as one
// demonstrates an isolation it does not have and every assertion still passes.
// And pgvector's binary codec is registered per connection, so without it
// embeddings still move — as text, several times slower — and nothing errors.
//
// Neither shows up as a failure anywhere downstream. So they are asserted here,
// once, before either example is written.
//
//	go run ./verify
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/mind-vm/sqlb"

	"github.com/mind-vm/ragit"
	"github.com/mind-vm/ragit/examples/fixtures"
	"github.com/mind-vm/ragit/examples/internal/bootstrap"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "\nverify:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg, err := bootstrap.LoadConfig()
	if err != nil {
		return err
	}

	fmt.Printf("admin dsn   %s\n", redactDSN(cfg.AdminDSN))
	fmt.Printf("app role    %s\n", cfg.AppRole)
	fmt.Printf("xberg       %s\n", cfg.XbergURL)
	fmt.Printf("minio       %s/%s\n", cfg.MinIOEndpoint, cfg.MinIOBucket)
	fmt.Printf("dimension   %d\n\n", cfg.EmbeddingDim)

	env, err := bootstrap.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	defer env.Close()

	checks := []struct {
		name string
		fn   func(context.Context, *bootstrap.Env) error
	}{
		{"schema: both migration lines applied", checkSchema},
		{"role: the app role cannot bypass RLS", checkRole},
		{"rls: confinement is the database's, not the query's", checkRLS},
		{"pgvector: the column width matches the configured dimension", checkDimension},
		{"pgvector: an embedding round-trips through the binary codec", checkVectorRoundTrip},
		{"host app: demo_uploads sits alongside ragit's tables", checkHostTable},
		{"store: MinIO round-trips an object", checkStore},
		{"xberg: the sidecar answers", checkXberg},
		{"fixtures: all three load", checkFixtures},
	}

	failed := 0
	for _, c := range checks {
		if err := c.fn(ctx, env); err != nil {
			fmt.Printf("FAIL  %s\n      %v\n", c.name, err)
			failed++
			continue
		}
		fmt.Printf("ok    %s\n", c.name)
	}

	fmt.Println()
	if failed > 0 {
		return fmt.Errorf("%d of %d checks failed", failed, len(checks))
	}
	fmt.Printf("all %d checks passed — the environment is ready for the examples\n", len(checks))
	return nil
}

// checkSchema confirms both migration lines ran: ragit's, tracked in its own
// version table, and the host application's, which knows nothing about it.
func checkSchema(ctx context.Context, env *bootstrap.Env) error {
	for _, table := range []string{"ragit_documents", "ragit_chunks", "ragit_migrations", "demo_uploads"} {
		var exists bool
		err := env.App.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = 'public' AND tablename = $1)",
			table).Scan(&exists)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("table %s is missing", table)
		}
	}
	return nil
}

// checkRole asserts the app role is unprivileged — and asserts the admin role
// is privileged, so a broken check cannot pass by always returning false.
func checkRole(ctx context.Context, env *bootstrap.Env) error {
	appPrivileged, err := bootstrap.IsSuperuser(ctx, env.App)
	if err != nil {
		return err
	}
	if appPrivileged {
		return fmt.Errorf("the app role is SUPERUSER or BYPASSRLS; every RLS policy is inert for it")
	}

	adminPrivileged, err := bootstrap.IsSuperuser(ctx, env.Admin)
	if err != nil {
		return err
	}
	if !adminPrivileged {
		// Not a problem for the examples, but it means this check is not
		// measuring what it claims to measure.
		return fmt.Errorf("the admin role is not privileged either, so this check proves nothing")
	}
	return nil
}

// checkRLS is the load-bearing one.
//
// It writes a row as one tenant and then reads it back four ways with raw SQL —
// not through ragit's query builder, because the claim under test is that the
// database confines the row regardless of who asks. Three of the four must come
// back empty; the fourth, as the superuser, must find it. Without that last
// read an empty table would produce three passes and prove nothing.
func checkRLS(ctx context.Context, env *bootstrap.Env) error {
	tenantA, tenantB := uuid.New(), uuid.New()

	id, err := insertDocument(ctx, env, tenantA, "rls-probe.md")
	if err != nil {
		return err
	}
	defer func() { _ = deleteDocument(ctx, env, tenantA, id) }()

	const countByID = "SELECT count(*) FROM ragit_documents WHERE id = $1"

	// As the owning tenant: visible.
	var n int
	if err := ragit.WithTenant(ctx, env.App, tenantA, func(db sqlb.Executor) error {
		return scanOne(ctx, db, countByID, []any{id}, &n)
	}); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("the owning tenant cannot see its own row (got %d)", n)
	}

	// As another tenant: invisible.
	if err := ragit.WithTenant(ctx, env.App, tenantB, func(db sqlb.Executor) error {
		return scanOne(ctx, db, countByID, []any{id}, &n)
	}); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("another tenant can see the row (got %d) — RLS is not confining reads", n)
	}

	// With no tenant set at all: invisible. FORCE ROW LEVEL SECURITY makes the
	// policy fail closed, so forgetting the scope under-fetches rather than
	// leaking.
	if err := env.App.QueryRow(ctx, countByID, id).Scan(&n); err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("an unscoped connection can see the row (got %d) — the policy is not failing closed", n)
	}

	// As the superuser, unscoped: visible, because PostgreSQL exempts
	// superusers from RLS entirely. This is the control: it proves the three
	// empty results above came from the policy and not from an empty table.
	if err := env.Admin.QueryRow(ctx, countByID, id).Scan(&n); err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("the superuser cannot see the row either (got %d) — the row was never written, so this check proved nothing", n)
	}
	return nil
}

// checkDimension compares the declared vector width against the configured one.
//
// A mismatch is the failure mode the dimension fork produces: the migrations
// ship vector(1536), and an example configured for anything else fails on its
// first insert, deep inside a batch, rather than here.
func checkDimension(ctx context.Context, env *bootstrap.Env) error {
	var declared string
	err := env.App.QueryRow(ctx, `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		WHERE c.relname = 'ragit_chunks' AND a.attname = 'embedding'`).Scan(&declared)
	if err != nil {
		return err
	}
	want := fmt.Sprintf("vector(%d)", env.Cfg.EmbeddingDim)
	if declared != want {
		return fmt.Errorf("ragit_chunks.embedding is %s but RAG_EMBEDDING_DIM asks for %s; "+
			"regenerate with `go run ./cmd/ragit-gen -dim %d -force` against a fresh database",
			declared, want, env.Cfg.EmbeddingDim)
	}
	return nil
}

// checkVectorRoundTrip writes an embedding and reads it back.
//
// Values are compared exactly rather than approximately: float32 through
// pgvector's binary format is lossless, and a codec that silently fell back to
// the text form would be the thing this is looking for.
func checkVectorRoundTrip(ctx context.Context, env *bootstrap.Env) error {
	tenant := uuid.New()

	docID, err := insertDocument(ctx, env, tenant, "vector-probe.md")
	if err != nil {
		return err
	}
	defer func() { _ = deleteDocument(ctx, env, tenant, docID) }()

	want := make(sqlb.Vector, env.Cfg.EmbeddingDim)
	for i := range want {
		want[i] = rand.Float32()
	}

	row := &ragit.Chunk{
		DocumentID:  docID,
		TenantID:    tenant,
		ChunkIndex:  0,
		HeadingPath: []string{"Probe"},
		Content:     "a chunk written by verify",
		Embedding:   &want,
		Metadata:    json.RawMessage("{}"),
		Attributes:  json.RawMessage("{}"),
	}

	var got sqlb.Vector
	err = ragit.WithTenant(ctx, env.App, tenant, func(db sqlb.Executor) error {
		created, err := sqlb.InsertRows(row).
			Omit("id", "created_at", "metadata", "attributes").
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("insert chunk: %w", err)
		}
		return scanOne(ctx, db,
			"SELECT embedding FROM ragit_chunks WHERE id = $1", []any{created.ID}, &got)
	})
	if err != nil {
		return err
	}

	if len(got) != len(want) {
		return fmt.Errorf("read back %d components, wrote %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("component %d changed in the round trip: wrote %v, read %v", i, want[i], got[i])
		}
	}
	return nil
}

// checkHostTable writes a row in the host application's own table pointing at
// a ragit document, which is the shape every consumer ends up with.
func checkHostTable(ctx context.Context, env *bootstrap.Env) error {
	tenant := uuid.New()

	docID, err := insertDocument(ctx, env, tenant, "host-probe.md")
	if err != nil {
		return err
	}
	defer func() { _ = deleteDocument(ctx, env, tenant, docID) }()

	upload := &bootstrap.Upload{
		TenantID:   tenant,
		DocumentID: &docID,
		Filename:   "host-probe.md",
		UploadedBy: "verify",
	}

	created, err := sqlb.InsertRows(upload).
		Omit("id", "created_at", "updated_at").
		One(ctx, env.App)
	if err != nil {
		return fmt.Errorf("insert demo_uploads row: %w", err)
	}
	defer func() {
		_, _ = env.App.Exec(ctx, "DELETE FROM demo_uploads WHERE id = $1", created.ID)
	}()

	// The join across the library boundary: the host table is not confined by
	// RLS, so this read needs no tenant transaction — but the ragit side of
	// the join does, which is exactly the asymmetry a consumer has to hold in
	// their head.
	var filename string
	err = ragit.WithTenant(ctx, env.App, tenant, func(db sqlb.Executor) error {
		return scanOne(ctx, db, `
			SELECT d.filename
			FROM demo_uploads u
			JOIN ragit_documents d ON d.id = u.document_id
			WHERE u.id = $1`, []any{created.ID}, &filename)
	})
	if err != nil {
		return fmt.Errorf("join demo_uploads to ragit_documents: %w", err)
	}
	if filename != "host-probe.md" {
		return fmt.Errorf("joined to the wrong document: %q", filename)
	}
	return nil
}

func checkStore(ctx context.Context, env *bootstrap.Env) error {
	tenant := uuid.New()
	want := []byte("verify probe")

	uri, err := env.Store.Put(ctx, tenant, "probe.txt", want, "text/plain")
	if err != nil {
		return err
	}
	defer func() { _ = env.Store.Delete(ctx, uri) }()

	rc, err := env.Store.Get(ctx, uri)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	if string(got) != string(want) {
		return fmt.Errorf("read back %q, wrote %q", got, want)
	}
	return nil
}

// checkXberg confirms the sidecar answers and reports what it is.
//
// The version matters more than it looks: xberg is on a 1.0.0-rc line, and the
// chunk-and-embed request shape the xberg-owned example depends on is the part
// most likely to have moved.
func checkXberg(ctx context.Context, env *bootstrap.Env) error {
	base := strings.TrimRight(env.Cfg.XbergURL, "/")

	if err := getOK(ctx, base+"/health"); err != nil {
		return fmt.Errorf("%w (is the xberg service up? it downloads models on first boot)", err)
	}

	body, err := get(ctx, base+"/info")
	if err != nil {
		return err
	}
	fmt.Printf("      xberg /info: %s\n", truncate(compactJSON(body), 200))
	return nil
}

func checkFixtures(_ context.Context, _ *bootstrap.Env) error {
	docs, err := fixtures.All()
	if err != nil {
		return err
	}
	if len(docs) != 3 {
		return fmt.Errorf("expected 3 fixtures, got %d", len(docs))
	}
	for _, d := range docs {
		if len(d.Data) == 0 {
			return fmt.Errorf("fixture %s is empty", d.Filename)
		}
	}
	return nil
}

// insertDocument writes a minimal document row directly, without going through
// the object store — these probes are about the database, and a Processor would
// drag extraction and embedding in with it.
func insertDocument(ctx context.Context, env *bootstrap.Env, tenant uuid.UUID, filename string) (uuid.UUID, error) {
	row := &ragit.Document{
		TenantID:   tenant,
		Filename:   filename,
		MimeType:   "text/markdown",
		Status:     ragit.StatusPending,
		Metadata:   json.RawMessage("{}"),
		Attributes: json.RawMessage("{}"),
	}

	var id uuid.UUID
	err := ragit.WithTenant(ctx, env.App, tenant, func(db sqlb.Executor) error {
		created, err := sqlb.InsertRows(row).
			Omit("id", "created_at", "updated_at", "status", "metadata", "attributes").
			One(ctx, db)
		if err != nil {
			return fmt.Errorf("insert document: %w", err)
		}
		id = created.ID
		return nil
	})
	return id, err
}

func deleteDocument(ctx context.Context, env *bootstrap.Env, tenant, id uuid.UUID) error {
	return ragit.WithTenant(ctx, env.App, tenant, func(db sqlb.Executor) error {
		_, err := db.Exec(ctx, "DELETE FROM ragit_documents WHERE id = $1", id)
		return err
	})
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: http %d: %s", url, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func getOK(ctx context.Context, url string) error {
	_, err := get(ctx, url)
	return err
}

func compactJSON(b []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		return strings.TrimSpace(string(b))
	}
	return out.String()
}

// scanOne reads exactly one row. sqlb.Executor is Query+Exec only — it has no
// QueryRow — and these probes deliberately use raw SQL rather than the query
// builder, because what they are testing is that the database confines a query
// it did not build.
func scanOne(ctx context.Context, db sqlb.Executor, sql string, args []any, dest ...any) error {
	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// redactDSN drops the password before anything is printed.
func redactDSN(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	scheme := strings.Index(dsn, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return dsn
	}
	return dsn[:scheme+3] + "***@" + dsn[at+1:]
}
