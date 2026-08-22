// Package ragitschema is ragit's schema declaration: the single source of
// truth from which its migrations and models are generated.
//
// It lives in its own package because the declarations here and the model
// structs generated from them share names — ragitschema.Document is the table
// declaration, ragit.Document is the row struct.
//
// # Why this is a function and not a set of package-level vars
//
// The embedding dimension is a column type, not a runtime setting: a
// vector(1536) column refuses a 768-component value, and changing it means
// re-embedding the whole corpus. Declaring the schema through [New] makes the
// dimension a value passed in rather than a literal baked into a shipped .sql
// file that a consumer would have to fork. See ragit.Migrate for how a
// deployment that wants a different width renders its own migration set.
package ragitschema

import "github.com/jryannel/sqlb/schema"

// DefaultEmbeddingDimension is the width ragit's shipped migrations declare.
// It matches embed.DefaultDimension.
const DefaultEmbeddingDimension = 1536

// ModuleName prefixes every table ragit owns. sqlb applies it at the registry
// rather than in each declaration, so table ownership is visible in the
// database and cannot be forgotten — which is the point, since these tables
// live in a host application's schema alongside its own and River's.
const ModuleName = "ragit"

// Schema holds ragit's declaration and the tables within it.
type Schema struct {
	Registry *schema.Registry
	Document *schema.TableDef
	Chunk    *schema.TableDef
	// Dimension is the embedding width this declaration was built for.
	Dimension int
}

// New builds the schema for a given embedding dimension.
//
// A fresh registry per call rather than package-level vars: two callers
// wanting different dimensions (a test and a deployment, say) must not fight
// over one global, and sqlb panics on a table registered twice.
func New(dimension int) *Schema {
	if dimension <= 0 {
		dimension = DefaultEmbeddingDimension
	}
	reg := schema.NewModule(ModuleName)

	document := reg.Table("documents",
		// gen_random_uuid() rather than sqlb's default UUIDv7. A time-ordered
		// v7 key has better index locality and would be the better choice in
		// an application, but it needs either PostgreSQL 18's built-in
		// uuidv7() or the pg_uuidv7 extension. ragit is a library dropped into
		// a database it does not control, so it takes the version floor that
		// asks least: gen_random_uuid() is built in from PostgreSQL 13 and
		// needs no extension at all. A deployment that wants v7 can regenerate
		// with migrate.MinPostgres(18) — it is a column default, so it changes
		// nothing about rows already written.
		schema.UUID("id").PrimaryKey().Default(schema.GenUUIDv4()),

		// tenant_id is Scoped: it is the confinement column, and marking it
		// here is what lets a BeforeQuery hook constrain every read of this
		// table — including reads written later, by code that never thought
		// about tenancy. See ragit.Confine.
		//
		// ReadOnly is required of a Scoped column by sqlb's schema validator,
		// and it polices the REST layer only: application code — ragit's own
		// inserts — still writes it.
		schema.UUID("tenant_id").Scoped().ReadOnly().Filterable(),

		// Two generic scope columns rather than one. A single scope id cannot
		// express a pair whose halves have independent lifecycles — the
		// motivating case is a corpus scoped by company AND by author, where
		// the author's material follows the author across companies. ragit
		// does not know what they mean; a host application maps its own
		// domain onto them.
		schema.UUID("scope_a_id").Nullable().Filterable(),
		schema.UUID("scope_b_id").Nullable().Filterable(),

		// session_id marks an ephemeral attachment belonging to one
		// conversation or agent session, excluded from ordinary library
		// search unless a caller names its session.
		schema.UUID("session_id").Nullable().Filterable(),

		schema.Text("source_uri").Nullable(),
		schema.Text("filename").Filterable().Sortable(),
		schema.Text("mime_type").Filterable(),
		schema.Text("status").Default(schema.Value("pending")).Filterable().Sortable().
			Comment("pending|processing|ready|error|skipped_too_large"),
		schema.Text("error").Nullable(),
		schema.Text("text_content").Nullable(),
		schema.JSON("metadata").Default(schema.Value("{}")),
		schema.Int("chunk_count").Nullable().Sortable(),
		schema.Text("embedding_model").Nullable().Filterable(),
		schema.Timestamp("processed_at").Nullable().Sortable(),

		// Retention clock. NULL means "keep until deleted", so a durable
		// library document is simply one that never set it.
		schema.Timestamp("expires_at").Nullable().Filterable(),

		schema.Timestamps(),
	).Describe("A source document ingested by ragit.")

	document.Index("tenant_id")
	document.Index("tenant_id", "scope_a_id")
	document.Index("tenant_id", "scope_b_id")
	document.AddIndex(schema.Index{
		Name:    "idx_ragit_documents_expires_at",
		Columns: []string{"expires_at"},
		Where:   "expires_at IS NOT NULL",
	})

	chunk := reg.Table("chunks",
		schema.UUID("id").PrimaryKey().Default(schema.GenUUIDv4()),
		schema.Ref("document", document).OnDelete(schema.Cascade),

		// tenant_id, the scope pair and session_id are denormalized copies of
		// the parent document's, snapshotted at processing time so retrieval
		// never needs a join to filter. That snapshot does not self-heal:
		// moving a document between scopes requires an explicit resync,
		// because the resume check sees identical content and skips
		// rewriting the row. See ragit.Processor.MoveDocumentScope.
		schema.UUID("tenant_id").Scoped().ReadOnly().Filterable(),
		schema.UUID("scope_a_id").Nullable().Filterable(),
		schema.UUID("scope_b_id").Nullable().Filterable(),
		schema.UUID("session_id").Nullable().Filterable(),

		schema.Int("chunk_index").Filterable().Sortable(),
		schema.Text("heading_path").Array().Nullable(),
		schema.Text("content"),

		// The dimension is the whole reason this file is a function.
		schema.Vector("embedding", dimension).Nullable(),

		// provider|model|dimension. Retrieval filters on it: cosine distance
		// between vectors from different models is not a weaker signal but a
		// meaningless one.
		schema.Text("embedding_fingerprint").Nullable().Filterable(),

		schema.JSON("metadata").Default(schema.Value("{}")),
		schema.Timestamp("expires_at").Nullable().Filterable(),
		schema.Timestamp("created_at").Default(schema.Now()),
	).Describe("One retrieval-sized piece of a document, with its embedding.")

	chunk.Index("tenant_id")
	chunk.Index("document_id")
	chunk.Index("tenant_id", "embedding_fingerprint")
	chunk.Index("tenant_id", "scope_a_id")
	chunk.Index("tenant_id", "scope_b_id")
	chunk.AddIndex(schema.Index{
		Name:    "idx_ragit_chunks_session_id",
		Columns: []string{"session_id"},
		Where:   "session_id IS NOT NULL",
	})
	chunk.AddIndex(schema.Index{
		Name:    "idx_ragit_chunks_expires_at",
		Columns: []string{"expires_at"},
		Where:   "expires_at IS NOT NULL",
	})

	// The HNSW index. pgvector's hnsw has no default operator class — the
	// class is what selects the distance function — so an index emitted
	// without one is rejected by Postgres outright.
	chunk.AddIndex(schema.Index{
		Name:      "idx_ragit_chunks_embedding_hnsw",
		Columns:   []string{"embedding"},
		Method:    "hnsw",
		Opclasses: map[string]string{"embedding": "vector_cosine_ops"},
		With:      map[string]string{"m": "16", "ef_construction": "64"},
	})

	return &Schema{Registry: reg, Document: document, Chunk: chunk, Dimension: dimension}
}
