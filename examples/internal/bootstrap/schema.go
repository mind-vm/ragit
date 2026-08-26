package bootstrap

import (
	"time"

	"github.com/google/uuid"
	"github.com/mind-vm/sqlb/schema"
)

// The host application's own table, declared with sqlb exactly as ragit's is.
//
// It exists so the examples are host applications rather than CLI wrappers
// around a library. The premise of ragit is that its tables live inside
// somebody else's database next to somebody else's tables, and none of the
// consequences of that — a second migration line, two table prefixes, a join
// across the boundary — show up in an example that only ever talks to ragit.
//
// demo_uploads is what an application would actually keep: who uploaded what,
// under which of its own identifiers, and which ragit document it became.
// ragit deliberately does not model any of that.

// DemoModule prefixes the host application's tables, the way ragit's own
// ModuleName prefixes ragit_. Two prefixes in one database is the point.
const DemoModule = "demo"

// DemoSchema is the host application's declaration.
type DemoSchema struct {
	Registry *schema.Registry
	Upload   *schema.TableDef
}

// NewDemoSchema builds it. A fresh registry per call, for the same reason
// ragitschema.New takes one: sqlb panics on a table registered twice.
func NewDemoSchema() *DemoSchema {
	reg := schema.NewModule(DemoModule)

	upload := reg.Table("uploads",
		schema.UUID("id").PrimaryKey().Default(schema.GenUUIDv4()),

		// tenant_id is filterable but deliberately NOT Scoped(), and this
		// table carries no RLS policy.
		//
		// That is not an oversight and not a recommendation — it is scope
		// control. ragit's confinement is what these examples are measuring,
		// and giving the host table its own independent isolation layer would
		// make it ambiguous which one produced any given empty result set. A
		// real application should confine its own tables too.
		schema.UUID("tenant_id").Filterable(),

		// The join back across the library boundary. Not a foreign key:
		// ragit owns ragit_documents and its migration line, so a constraint
		// from this table into it would couple two independent schemas and
		// make the drop order matter.
		schema.UUID("document_id").Nullable().Filterable(),

		schema.Text("filename").Filterable().Sortable(),
		schema.Text("uploaded_by").Filterable(),
		schema.Timestamps(),
	)

	return &DemoSchema{Registry: reg, Upload: upload}
}

// Upload is the row struct for demo_uploads.
//
// Hand-written rather than generated: it is eight columns in an example, and a
// codegen step here would obscure what the example is demonstrating. ragit
// generates its models because its schema is the product; this one is scaffolding.
type Upload struct {
	ID         uuid.UUID  `db:"id" json:"id" sqlb:"type:uuid,pk,default,filter,readonly"`
	TenantID   uuid.UUID  `db:"tenant_id" json:"tenant_id" sqlb:"type:uuid,filter"`
	DocumentID *uuid.UUID `db:"document_id" json:"document_id" sqlb:"type:uuid,filter"`
	Filename   string     `db:"filename" json:"filename" sqlb:"type:text,filter,sort"`
	UploadedBy string     `db:"uploaded_by" json:"uploaded_by" sqlb:"type:text,filter"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at" sqlb:"type:timestamptz,default,sort,readonly"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at" sqlb:"type:timestamptz,default,sort,readonly"`
}

// TableName is the table Upload maps to.
func (Upload) TableName() string { return DemoModule + "_uploads" }
